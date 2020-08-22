package jx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"text/template"

	jxv1 "github.com/jenkins-x/jx-api/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/v2/pkg/tekton/metapipeline"
	lighthousev1alpha1 "github.com/jenkins-x/lighthouse/pkg/apis/lighthouse/v1alpha1"
	configjob "github.com/jenkins-x/lighthouse/pkg/config/job"
	"github.com/jenkins-x/lighthouse/pkg/util"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sigs.k8s.io/yaml"
)

const (
	controllerName           = "jx-controller"
	defaultTargetURLTemplate = "{{ .BaseURL }}/teams/{{ .Team }}/projects/{{ .Owner }}/{{ .Repository }}/{{ .Branch }}/{{ .Build }}"
	baseTargetURLEnvVar      = "LIGHTHOUSE_REPORT_URL_BASE"
	targetURLTeamEnvVar      = "LIGHTHOUSE_REPORT_URL_TEAM"
	pipelineActivityKey      = ".metadata.pipelineActivity"
)

// LighthouseJobReconciler reconciles a LighthouseJob object
type LighthouseJobReconciler struct {
	client    client.Client
	logger    *logrus.Entry
	scheme    *runtime.Scheme
	namespace string
	mpClient  metapipeline.Client
}

// NewLighthouseJobReconciler creates a LighthouseJob reconciler
func NewLighthouseJobReconciler(client client.Client, scheme *runtime.Scheme, namespace string, mpClient metapipeline.Client) (*LighthouseJobReconciler, error) {
	if mpClient == nil {
		_mpClient, _, _, err := NewMetaPipelineClient(namespace)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create metapipeline client")
		}
		mpClient = _mpClient
	}
	return &LighthouseJobReconciler{
		client:    client,
		logger:    logrus.NewEntry(logrus.StandardLogger()).WithField("controller", controllerName),
		scheme:    scheme,
		namespace: namespace,
		mpClient:  mpClient,
	}, nil
}

// SetupWithManager sets up the reconcilier with it's manager
func (r *LighthouseJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(&jxv1.PipelineActivity{}, pipelineActivityKey, func(rawObj runtime.Object) []string {
		obj := rawObj.(*jxv1.PipelineActivity)
		return []string{util.ToValidName(obj.Name)}
	}); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(&lighthousev1alpha1.LighthouseJob{}, pipelineActivityKey, func(rawObj runtime.Object) []string {
		obj := rawObj.(*lighthousev1alpha1.LighthouseJob)
		if obj.Status.ActivityName == "" {
			return nil
		}
		return []string{obj.Status.ActivityName}
	}); err != nil {
		return err
	}
	c, err := controller.New(controllerName, mgr, controller.Options{
		Reconciler: r,
	})
	if err != nil {
		return err
	}
	if err := c.Watch(
		&source.Kind{Type: &lighthousev1alpha1.LighthouseJob{}},
		&handler.EnqueueRequestForObject{},
	); err != nil {
		return err
	}
	if err := c.Watch(
		&source.Kind{Type: &jxv1.PipelineActivity{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(func(o handler.MapObject) []reconcile.Request {
				var jobList lighthousev1alpha1.LighthouseJobList
				if err := r.client.List(nil, &jobList, client.InNamespace(o.Meta.GetNamespace()), client.MatchingFields{pipelineActivityKey: util.ToValidName(o.Meta.GetName())}); err != nil {
					r.logger.Errorf("Failed list jobs: %s", err)
					return nil
				}
				r.logger.Infof("Found jobs for activity %+v: %+v", o.Meta, jobList)
				var requests []ctrl.Request
				for _, job := range jobList.Items {
					requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
						Name:      job.Name,
						Namespace: job.Namespace,
					}})
				}
				return requests
			}),
		},
		predicate.ResourceVersionChangedPredicate{},
	); err != nil {
		return err
	}
	return nil
}

// Reconcile represents an iteration of the reconciliation loop
func (r *LighthouseJobReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()

	r.logger.Infof("Reconcile LighthouseJob %+v", req)

	// get lighthouse job
	var job lighthousev1alpha1.LighthouseJob
	if err := r.client.Get(ctx, req.NamespacedName, &job); err != nil {
		r.logger.Warningf("Unable to get LighthouseJob: %s", err)
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// filter on job agent
	if job.Spec.Agent != configjob.JenkinsXAgent && job.Spec.Agent != configjob.LegacyDefaultAgent {
		return ctrl.Result{}, nil
	}

	// get job's pipeline activities
	var pipelineActivityList jxv1.PipelineActivityList
	if err := r.client.List(ctx, &pipelineActivityList, client.InNamespace(req.Namespace), client.MatchingFields{pipelineActivityKey: job.Status.ActivityName}); err != nil {
		r.logger.Errorf("Failed list pipeline runs: %s", err)
		return ctrl.Result{}, err
	}

	// if pipeline activity does not exist, create it
	if len(pipelineActivityList.Items) == 0 {
		if job.Status.State == lighthousev1alpha1.TriggeredState {
			jobName := job.Spec.Refs.Repo
			owner := job.Spec.Refs.Org
			sourceURL := job.Spec.Refs.CloneURI
			pullRefData := getPullRefs(sourceURL, &job.Spec)
			branch := job.Spec.GetBranch()
			pullRefs := ""
			sa := os.Getenv("JX_SERVICE_ACCOUNT")
			kind := metapipeline.ReleasePipeline

			if branch == "" {
				branch = "master"
			}
			if len(job.Spec.Refs.Pulls) > 0 {
				pullRefs = pullRefData.String()
			}
			if pullRefs == "" {
				pullRefs = branch + ":"
			}
			if sa == "" {
				sa = "tekton-bot"
			}
			if len(job.Spec.Refs.Pulls) > 0 {
				kind = metapipeline.PullRequestPipeline
			}

			l := logrus.WithFields(logrus.Fields(map[string]interface{}{
				"Owner":     owner,
				"Name":      jobName,
				"SourceURL": sourceURL,
				"Branch":    branch,
				"PullRefs":  pullRefs,
				"Job":       job.Spec.Job,
			}))

			l.Info("about to start Jenkinx X meta pipeline")

			pipelineCreateParam := metapipeline.PipelineCreateParam{
				PullRef:      pullRefData,
				PipelineKind: kind,
				Context:      job.Spec.Context,
				// No equivalent to https://github.com/jenkins-x/jx/blob/bb59278c2707e0e99b3c24be926745c324824388/pkg/cmd/controller/pipeline/pipelinerunner_controller.go#L236
				//   for getting environment variables from the prow job here, so far as I can tell (abayer)
				// Also not finding an equivalent to labels from the PipelineRunRequest
				ServiceAccount: sa,
				// I believe we can use an empty string default image?
				DefaultImage: os.Getenv("JX_DEFAULT_IMAGE"),
				EnvVariables: job.Spec.GetEnvVars(),
			}

			activityKey, tektonCRDs, err := r.mpClient.Create(pipelineCreateParam)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "unable to create Tekton CRDs")
			}
			job.Status = lighthousev1alpha1.LighthouseJobStatus{
				State:        lighthousev1alpha1.PendingState,
				ActivityName: util.ToValidName(activityKey.Name),
				StartTime:    metav1.Now(),
			}
			if err := r.client.Status().Update(ctx, &job); err != nil {
				r.logger.Errorf("Failed to update LighthouseJob status: %s", err)
				return ctrl.Result{}, err
			}
			err = r.mpClient.Apply(activityKey, tektonCRDs)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "unable to apply Tekton CRDs")
			}
		}
	} else if len(pipelineActivityList.Items) == 1 {
		// if pipeline run exists, create it and update status
		pipelineActivity := pipelineActivityList.Items[0]
		// update build id
		job.Labels[util.BuildNumLabel] = pipelineActivity.Spec.Build
		if err := r.client.Update(ctx, &job); err != nil {
			r.logger.Errorf("failed to update Project status: %s", err)
			return ctrl.Result{}, err
		}
		activityRecord, err := ConvertPipelineActivity(&pipelineActivity)
		if err != nil {
			return ctrl.Result{}, err
		}
		urlBase := getReportURLBase()
		if urlBase != "" {
			urlTeam := getReportURLTeam()
			team := r.namespace
			// override with env var if set
			if urlTeam != "" {
				team = urlTeam
			}

			pipelineContext := activityRecord.Context
			if pipelineContext == "" {
				pipelineContext = "jenkins-x"
			}

			targetURL := createReportTargetURL(defaultTargetURLTemplate, r.logger, ReportParams{
				Owner:      activityRecord.Owner,
				Repository: activityRecord.Repo,
				Branch:     activityRecord.Branch,
				Build:      activityRecord.BuildIdentifier,
				Context:    pipelineContext,
				// TODO: Need to get the job URL base in here somehow. (apb)
				BaseURL: strings.TrimRight(urlBase, "/"),
				Team:    team,
			})

			if strings.HasPrefix(targetURL, "http://") || strings.HasPrefix(targetURL, "https://") {
				job.Status.ReportURL = targetURL
			}
		}
		job.Status.Activity = activityRecord
		if err := r.client.Status().Update(ctx, &job); err != nil {
			r.logger.Errorf("Failed to update LighthouseJob status: %s", err)
			return ctrl.Result{}, err
		}
	} else {
		r.logger.Errorf("A lighthouse job should never have more than 1 pipeline activity")
	}

	return ctrl.Result{}, nil
}

func getPullRefs(sourceURL string, spec *lighthousev1alpha1.LighthouseJobSpec) metapipeline.PullRef {
	var pullRef metapipeline.PullRef
	if len(spec.Refs.Pulls) > 0 {
		var prs []metapipeline.PullRequestRef
		for _, pull := range spec.Refs.Pulls {
			prs = append(prs, metapipeline.PullRequestRef{ID: strconv.Itoa(pull.Number), MergeSHA: pull.SHA})
		}

		pullRef = metapipeline.NewPullRefWithPullRequest(sourceURL, spec.Refs.BaseRef, spec.Refs.BaseSHA, prs...)
	} else {
		pullRef = metapipeline.NewPullRef(sourceURL, spec.Refs.BaseRef, spec.Refs.BaseSHA)
	}

	return pullRef
}

// getReportURLBase gets the base report URL from the environment
func getReportURLBase() string {
	return os.Getenv(baseTargetURLEnvVar)
}

// getReportURLTeam gets the team to construct the report url
func getReportURLTeam() string {
	return os.Getenv(targetURLTeamEnvVar)
}

// ReportParams contains the parameters for target URL templates
type ReportParams struct {
	BaseURL, Owner, Repository, Branch, Build, Context, Team string
}

// createReportTargetURL creates the target URL for pipeline results/logs from a template
func createReportTargetURL(templateText string, logger *logrus.Entry, params ReportParams) string {
	templateData, err := toObjectMap(params)
	if err != nil {
		logger.WithError(err).Warnf("failed to convert git ReportParams to a map for %#v", params)
		return ""
	}

	tmpl, err := template.New("target_url.tmpl").Option("missingkey=error").Parse(templateText)
	if err != nil {
		logger.WithError(err).Warnf("failed to parse git ReportsParam template: %s", templateText)
		return ""
	}

	var buf strings.Builder
	err = tmpl.Execute(&buf, templateData)
	if err != nil {
		logger.WithError(err).Warnf("failed to evaluate git ReportsParam template: %s due to: %s", templateText, err.Error())
		return ""
	}
	return buf.String()
}

// toObjectMap converts the given object into a map of strings/maps using YAML marshalling
func toObjectMap(object interface{}) (map[string]interface{}, error) {
	answer := map[string]interface{}{}
	data, err := yaml.Marshal(object)
	if err != nil {
		return answer, err
	}
	err = yaml.Unmarshal(data, &answer)
	return answer, err
}
