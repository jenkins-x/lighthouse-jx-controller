package jx

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	jxv1 "github.com/jenkins-x/jx-api/pkg/apis/jenkins.io/v1"
	jxclient "github.com/jenkins-x/jx-api/pkg/client/clientset/versioned"
	jxfake "github.com/jenkins-x/jx-api/pkg/client/clientset/versioned/fake"
	"github.com/jenkins-x/jx/v2/pkg/gits"
	"github.com/jenkins-x/jx/v2/pkg/kube"
	"github.com/jenkins-x/jx/v2/pkg/tekton"
	"github.com/jenkins-x/jx/v2/pkg/tekton/metapipeline"
	"github.com/jenkins-x/jx/v2/pkg/util"
	lighthousev1alpha1 "github.com/jenkins-x/lighthouse/pkg/apis/lighthouse/v1alpha1"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

type fakeMetapipelineClient struct {
	jxClient jxclient.Interface
	client   client.Client
	ns       string
}

// Create just creates a PipelineActivity key
func (f *fakeMetapipelineClient) Create(param metapipeline.PipelineCreateParam) (kube.PromoteStepActivityKey, tekton.CRDWrapper, error) {
	gitInfo, err := gits.ParseGitURL(param.PullRef.SourceURL())
	if err != nil {
		return kube.PromoteStepActivityKey{}, tekton.CRDWrapper{}, errors.Wrap(err, fmt.Sprintf("unable to determine needed git info from the specified git url '%s'", param.PullRef.SourceURL()))
	}

	var branchIdentifier string
	switch param.PipelineKind {
	case metapipeline.ReleasePipeline:
		// no pull requests to merge, taking base branch name as identifier
		branchIdentifier = param.PullRef.BaseBranch()
	case metapipeline.PullRequestPipeline:
		if len(param.PullRef.PullRequests()) == 0 {
			return kube.PromoteStepActivityKey{}, tekton.CRDWrapper{}, errors.New("pullrequest pipeline requested, but no pull requests specified")
		}
		branchIdentifier = fmt.Sprintf("PR-%s", param.PullRef.PullRequests()[0].ID)
	default:
		branchIdentifier = "unknown"
	}

	pr, _ := tekton.ParsePullRefs(param.PullRef.String())
	pipelineActivity := tekton.GeneratePipelineActivity("1", branchIdentifier, gitInfo, param.Context, pr)

	return *pipelineActivity, tekton.CRDWrapper{}, nil
}

// Apply just applies the PipelineActivity
func (f *fakeMetapipelineClient) Apply(pipelineActivity kube.PromoteStepActivityKey, crds tekton.CRDWrapper) error {
	activity, _, err := pipelineActivity.GetOrCreate(f.jxClient, f.ns)
	if err != nil {
		return err
	}
	f.client.Create(nil, activity)
	return nil
}

// Close is a no-op here
func (f *fakeMetapipelineClient) Close() error {
	return nil
}

func TestReconcile(t *testing.T) {
	origBase := os.Getenv(baseTargetURLEnvVar)
	origTeam := os.Getenv(targetURLTeamEnvVar)
	os.Setenv(baseTargetURLEnvVar, "https://example.com")
	os.Setenv(targetURLTeamEnvVar, "some-team")
	defer func() {
		if origBase != "" {
			os.Setenv(baseTargetURLEnvVar, origBase)
		} else {
			os.Unsetenv(baseTargetURLEnvVar)
		}
		if origTeam != "" {
			os.Setenv(targetURLTeamEnvVar, origTeam)
		} else {
			os.Unsetenv(targetURLTeamEnvVar)
		}
	}()
	testCases := []string{
		"start-pullrequest",
		"update-job",
	}

	for _, tc := range testCases {
		t.Run(tc, func(t *testing.T) {
			testData := path.Join("test_data", "controller", tc)
			_, err := os.Stat(testData)
			assert.NoError(t, err)

			// load observed state
			ns := "jx"
			observedActivity, err := loadPipelineActivity(true, testData)
			assert.NoError(t, err)
			observedJob, err := loadLighthouseJob(true, testData)
			assert.NoError(t, err)
			var state []runtime.Object
			if observedActivity != nil {
				state = append(state, observedActivity)
			}
			if observedJob != nil {
				state = append(state, observedJob)
			}
			var jxObjects []runtime.Object
			if observedActivity != nil {
				jxObjects = append(jxObjects, observedActivity)
			}

			// load expected state
			expectedActivity, err := loadPipelineActivity(false, testData)
			assert.NoError(t, err)
			expectedJob, err := loadLighthouseJob(false, testData)
			assert.NoError(t, err)

			// create fake controller
			scheme := runtime.NewScheme()
			err = lighthousev1alpha1.AddToScheme(scheme)
			assert.NoError(t, err)
			err = jxv1.AddToScheme(scheme)
			assert.NoError(t, err)
			c := fake.NewFakeClientWithScheme(scheme, state...)
			jxClient := jxfake.NewSimpleClientset(jxObjects...)
			mpc := &fakeMetapipelineClient{
				jxClient: jxClient,
				client:   c,
				ns:       ns,
			}
			reconciler, err := NewLighthouseJobReconciler(c, scheme, ns, mpc)
			assert.NoError(t, err)

			// invoke reconcile
			jobName := "dummy"
			if observedJob != nil {
				jobName = observedJob.GetName()
			}
			_, err = reconciler.Reconcile(ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ns,
					Name:      jobName,
				},
			})
			assert.NoError(t, err)

			if expectedActivity != nil {
				var pipelineActivityList jxv1.PipelineActivityList
				err := c.List(nil, &pipelineActivityList, client.InNamespace(ns))
				assert.NoError(t, err)
				assert.Len(t, pipelineActivityList.Items, 1)
				updatedActivity := pipelineActivityList.Items[0].DeepCopy()
				if d := cmp.Diff(expectedActivity, updatedActivity); d != "" {
					t.Errorf("PipelineActivity did not match expected: %s", d)
				}
			}
			if expectedJob != nil {
				var jobList lighthousev1alpha1.LighthouseJobList
				err := c.List(nil, &jobList, client.InNamespace(ns))
				assert.NoError(t, err)
				assert.Len(t, jobList.Items, 1)
				// Ignore status.starttime since that's always going to be different
				updatedJob := jobList.Items[0].DeepCopy()
				updatedJob.Status.StartTime = metav1.Time{}
				if d := cmp.Diff(expectedJob.Status, updatedJob.Status); d != "" {
					t.Errorf("LighthouseJob did not match expected: %s", d)
				}
			}
		})
	}
}

func loadLighthouseJob(isObserved bool, dir string) (*lighthousev1alpha1.LighthouseJob, error) {
	var baseFn string
	if isObserved {
		baseFn = "observed-lhjob.yml"
	} else {
		baseFn = "expected-lhjob.yml"
	}
	fileName := filepath.Join(dir, baseFn)
	exists, err := util.FileExists(fileName)
	if err != nil {
		return nil, err
	}
	if exists {
		lhjob := &lighthousev1alpha1.LighthouseJob{}
		data, err := ioutil.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
		err = yaml.Unmarshal(data, lhjob)
		if err != nil {
			return nil, err
		}
		return lhjob, err
	}
	return nil, nil
}

func loadPipelineActivity(isObserved bool, dir string) (*jxv1.PipelineActivity, error) {
	var baseFn string
	if isObserved {
		baseFn = "observed-activity.yml"
	} else {
		baseFn = "expected-activity.yml"
	}
	fileName := filepath.Join(dir, baseFn)
	exists, err := util.FileExists(fileName)
	if err != nil {
		return nil, err
	}
	if exists {
		activity := &jxv1.PipelineActivity{}
		data, err := ioutil.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
		err = yaml.Unmarshal(data, activity)
		if err != nil {
			return nil, err
		}
		return activity, err
	}
	return nil, nil
}
