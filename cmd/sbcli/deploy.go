package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	ateclientset "github.com/agent-substrate/substrate/pkg/client/clientset/versioned"
	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// defaultPauseImage is the digest-pinned pause image recommended by the
// Substrate ActorTemplate documentation for off-GCP clusters.
const defaultPauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// apiName is the name of the REST API Deployment and Service.
const apiName = "substrate-sandbox-api"

// In-cluster addresses of the Substrate control plane and router, used by
// the deployed REST API.
const (
	inClusterAteapi = "ateapi.ate-system.svc:443"
	inClusterAtenet = "atenet-router.ate-system.svc:80"
)

type deployConfig struct {
	namespace       string
	template        string
	workerPool      string
	guestdImage     string
	ateomImage      string
	pauseImage      string
	snapshotsBucket string
	replicas        int32
	apiImage        string
	apiReplicas     int32
	guestdCommand   []string
	waitForReady    time.Duration
	poolLabels      map[string]string
	kubeconfig      string
	kubeContext     string
}

func newDeployCommand(namespace, template *string) *cobra.Command {
	cfg := deployConfig{}

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy the system to a cluster running Agent Substrate",
		Long: `Deploy creates everything sandboxes need on a Kubernetes cluster that
already runs the Agent Substrate system: the target namespace, a
WorkerPool of pre-warmed workers, the ActorTemplate that sandboxes are
created from, and the substrate-sandbox-api REST service.

Released sbcli binaries embed digest-pinned default images for the guest
daemon and the worker, so only --snapshots-bucket is required:

  sbcli deploy --snapshots-bucket gs://<bucket>/substrate-sandbox/

Images must be pinned by digest (repo@sha256:...); Substrate rejects
unpinned images because changing an image invalidates snapshots. To use
your own images (required when sbcli was built from source), build and
push them with ko:

  export KO_DOCKER_REPO=gcr.io/<your-project>
  sbcli deploy \
    --guestd-image $(ko build github.com/rakyll/substrate-sandbox/cmd/substrate-guestd) \
    --ateom-image  $(cd <substrate-checkout> && ko build ./cmd/ateom-gvisor) \
    --snapshots-bucket gs://<bucket>/substrate-sandbox/ \
    --template sandbox --namespace substrate-sandbox`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.resolveImages(); err != nil {
				return err
			}
			cfg.namespace = *namespace
			cfg.template = *template
			if cfg.template == "" {
				cfg.template = "sandbox"
			}
			if cfg.workerPool == "" {
				cfg.workerPool = cfg.template + "-workerpool"
			}
			cfg.poolLabels = map[string]string{"workload": cfg.template}

			kube, ate, err := kubeClients(cfg.kubeconfig, cfg.kubeContext)
			if err != nil {
				return err
			}
			return runDeploy(cmd.Context(), cmd, kube, ate, cfg)
		},
	}

	cmd.Flags().StringVar(&cfg.guestdImage, "guestd-image", defaultGuestdImage, "digest-pinned substrate-guestd image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.ateomImage, "ateom-image", defaultAteomImage, "digest-pinned ateom image for the worker pool, e.g. ateom-gvisor built from the Substrate repo")
	cmd.Flags().StringVar(&cfg.snapshotsBucket, "snapshots-bucket", "", "object-storage bucket (with optional prefix) for suspend snapshots, e.g. gs://bucket/prefix/")
	cmd.Flags().StringVar(&cfg.pauseImage, "pause-image", defaultPauseImage, "digest-pinned pause image for the root sandbox container")
	cmd.Flags().StringVar(&cfg.apiImage, "api-image", defaultAPIImage, "substrate-sandbox-api image for the REST service")
	cmd.Flags().Int32Var(&cfg.apiReplicas, "api-replicas", 1, "number of REST service replicas")
	cmd.Flags().StringVar(&cfg.workerPool, "workerpool", "", "WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().Int32Var(&cfg.replicas, "replicas", 2, "number of pre-warmed worker pods")
	cmd.Flags().StringSliceVar(&cfg.guestdCommand, "guestd-command", []string{"/ko-app/substrate-guestd", "-workdir", "/workspace"}, "guestd container entrypoint")
	cmd.Flags().DurationVar(&cfg.waitForReady, "wait", 5*time.Minute, "how long to wait for the ActorTemplate to become Ready (0 to skip)")
	cmd.Flags().StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to the kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&cfg.kubeContext, "kube-context", "", "kubeconfig context to use")
	cmd.MarkFlagRequired("snapshots-bucket")

	return cmd
}

// resolveImages verifies that all deployment images are set, either baked
// in at release time or passed as flags.
func (c *deployConfig) resolveImages() error {
	if c.guestdImage != "" && c.ateomImage != "" && c.apiImage != "" {
		return nil
	}
	return errors.New(`no default images are baked into this build of sbcli (they are set when
sbcli is built by a release); pass the images explicitly:

  export KO_DOCKER_REPO=<your-registry>
  sbcli deploy \
    --guestd-image $(ko build github.com/rakyll/substrate-sandbox/cmd/substrate-guestd) \
    --api-image    $(ko build github.com/rakyll/substrate-sandbox/cmd/substrate-sandbox-api) \
    --ateom-image  $(cd <substrate-checkout> && ko build ./cmd/ateom-gvisor) \
    ...`)
}

func kubeClients(kubeconfig, kubeContext string) (kubernetes.Interface, ateclientset.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating Kubernetes client: %w", err)
	}
	ate, err := ateclientset.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating Substrate client: %w", err)
	}
	return kube, ate, nil
}

func runDeploy(ctx context.Context, cmd *cobra.Command, kube kubernetes.Interface, ate ateclientset.Interface, cfg deployConfig) error {
	if err := ensureNamespace(ctx, kube, cfg.namespace); err != nil {
		return err
	}
	cmd.Printf("namespace %s ready\n", cfg.namespace)

	if err := applyWorkerPool(ctx, ate, cfg); err != nil {
		return err
	}
	cmd.Printf("workerpool %s/%s applied (%d replicas)\n", cfg.namespace, cfg.workerPool, cfg.replicas)

	if err := applyActorTemplate(ctx, ate, cfg); err != nil {
		return err
	}
	cmd.Printf("actortemplate %s/%s applied\n", cfg.namespace, cfg.template)

	if err := applyAPI(ctx, kube, cfg); err != nil {
		return err
	}
	cmd.Printf("api %s/%s applied (%d replicas); reach it with: kubectl port-forward -n %s svc/%s 7777:7777\n",
		cfg.namespace, apiName, cfg.apiReplicas, cfg.namespace, apiName)

	if cfg.waitForReady <= 0 {
		return nil
	}
	cmd.Printf("waiting up to %s for actortemplate %s/%s to become Ready...\n", cfg.waitForReady, cfg.namespace, cfg.template)
	if err := waitTemplateReady(ctx, ate, cfg.namespace, cfg.template, cfg.waitForReady); err != nil {
		return err
	}
	cmd.Printf("actortemplate %s/%s is Ready; create sandboxes with: sbcli sandbox create <id> --template %s --namespace %s\n",
		cfg.namespace, cfg.template, cfg.template, cfg.namespace)
	return nil
}

func ensureNamespace(ctx context.Context, kube kubernetes.Interface, namespace string) error {
	_, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating namespace %q: %w", namespace, err)
	}
	return nil
}

func applyWorkerPool(ctx context.Context, ate ateclientset.Interface, cfg deployConfig) error {
	pool := &atev1alpha1.WorkerPool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.workerPool,
			Namespace: cfg.namespace,
			Labels:    cfg.poolLabels,
		},
		Spec: atev1alpha1.WorkerPoolSpec{
			Replicas:   cfg.replicas,
			AteomImage: cfg.ateomImage,
		},
	}
	client := ate.ApiV1alpha1().WorkerPools(cfg.namespace)
	_, err := client.Create(ctx, pool, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := client.Get(ctx, cfg.workerPool, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting workerpool %q: %w", cfg.workerPool, getErr)
		}
		existing.Labels = cfg.poolLabels
		existing.Spec = pool.Spec
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("applying workerpool %q: %w", cfg.workerPool, err)
	}
	return nil
}

func applyActorTemplate(ctx context.Context, ate ateclientset.Interface, cfg deployConfig) error {
	port := "80"
	template := &atev1alpha1.ActorTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.template,
			Namespace: cfg.namespace,
		},
		Spec: atev1alpha1.ActorTemplateSpec{
			PauseImage: cfg.pauseImage,
			WorkerSelector: &metav1.LabelSelector{
				MatchLabels: cfg.poolLabels,
			},
			Containers: []atev1alpha1.Container{{
				Name:    "guestd",
				Image:   cfg.guestdImage,
				Command: cfg.guestdCommand,
				Env: []atev1alpha1.EnvVar{{
					Name:  "PORT",
					Value: &port,
				}},
				Readyz: &atev1alpha1.ContainerReadyz{
					HTTPGet: &atev1alpha1.HTTPGetAction{
						Path: "/healthz",
						Port: 80,
					},
				},
			}},
			SnapshotsConfig: atev1alpha1.SnapshotsConfig{
				Location: cfg.snapshotsBucket,
			},
		},
	}
	client := ate.ApiV1alpha1().ActorTemplates(cfg.namespace)
	_, err := client.Create(ctx, template, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := client.Get(ctx, cfg.template, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting actortemplate %q: %w", cfg.template, getErr)
		}
		existing.Spec = template.Spec
		_, err = client.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("applying actortemplate %q: %w", cfg.template, err)
	}
	return nil
}

// applyAPI deploys the substrate-sandbox-api REST service: a Deployment
// pointed at the in-cluster Substrate endpoints and a Service exposing it
// on port 80.
func applyAPI(ctx context.Context, kube kubernetes.Interface, cfg deployConfig) error {
	labels := map[string]string{"app": apiName}
	replicas := cfg.apiReplicas
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiName,
			Namespace: cfg.namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  apiName,
						Image: cfg.apiImage,
						Args: []string{
							"-listen", "0.0.0.0:7777",
							"-ateapi", inClusterAteapi,
							"-atenet", inClusterAtenet,
						},
						Ports: []corev1.ContainerPort{{ContainerPort: 7777}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(7777),
								},
							},
						},
					}},
				},
			},
		},
	}
	deployments := kube.AppsV1().Deployments(cfg.namespace)
	_, err := deployments.Create(ctx, deployment, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := deployments.Get(ctx, apiName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting deployment %q: %w", apiName, getErr)
		}
		existing.Labels = labels
		existing.Spec = deployment.Spec
		_, err = deployments.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("applying deployment %q: %w", apiName, err)
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      apiName,
			Namespace: cfg.namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       7777,
				TargetPort: intstr.FromInt32(7777),
			}},
		},
	}
	services := kube.CoreV1().Services(cfg.namespace)
	_, err = services.Create(ctx, service, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := services.Get(ctx, apiName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting service %q: %w", apiName, getErr)
		}
		// Update in place to preserve the allocated ClusterIP.
		existing.Labels = labels
		existing.Spec.Selector = service.Spec.Selector
		existing.Spec.Ports = service.Spec.Ports
		_, err = services.Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("applying service %q: %w", apiName, err)
	}
	return nil
}

func waitTemplateReady(ctx context.Context, ate ateclientset.Interface, namespace, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastPhase atev1alpha1.PhaseType
	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		template, err := ate.ApiV1alpha1().ActorTemplates(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		lastPhase = template.Status.Phase
		if lastPhase == atev1alpha1.PhaseFailed {
			return false, fmt.Errorf("actortemplate %s/%s failed", namespace, name)
		}
		if lastPhase == atev1alpha1.PhaseReady {
			return true, nil
		}
		for _, cond := range template.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == metav1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for actortemplate %s/%s to become Ready (last phase %q): %w",
			namespace, name, lastPhase, err)
	}
	return nil
}
