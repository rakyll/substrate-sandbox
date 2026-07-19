package main

import (
	"context"
	"fmt"
	"time"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	ateclientset "github.com/agent-substrate/substrate/pkg/client/clientset/versioned"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// defaultPauseImage is the digest-pinned pause image recommended by the
// Substrate ActorTemplate documentation for off-GCP clusters.
const defaultPauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

type deployConfig struct {
	namespace         string
	template          string
	workerPool        string
	guestdImage       string
	ateomImage        string
	pauseImage        string
	snapshotsLocation string
	replicas          int32
	guestdCommand     []string
	waitForReady      time.Duration
	poolLabels        map[string]string
	kubeconfig        string
	kubeContext       string
}

func newDeployCommand(namespace, template *string) *cobra.Command {
	cfg := deployConfig{}

	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy the system to a cluster running Agent Substrate",
		Long: `Deploy creates everything sandboxes need on a Kubernetes cluster that
already runs the Agent Substrate system: the target namespace, a
WorkerPool of pre-warmed workers, and the ActorTemplate that sandboxes
are created from.

Images must be pinned by digest (repo@sha256:...); Substrate rejects
unpinned images because changing an image invalidates snapshots. Build
and push them with ko:

  export KO_DOCKER_REPO=gcr.io/<your-project>
  sbcli deploy \
    --guestd-image $(ko build github.com/rakyll/substrate-sandbox/cmd/substrate-guestd) \
    --ateom-image  $(cd <substrate-checkout> && ko build ./cmd/ateom-gvisor) \
    --snapshots-location gs://<bucket>/substrate-sandbox/ \
    --template sandbox --namespace substrate-sandbox`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
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

	cmd.Flags().StringVar(&cfg.guestdImage, "guestd-image", "", "digest-pinned substrate-guestd image (repo@sha256:...)")
	cmd.Flags().StringVar(&cfg.ateomImage, "ateom-image", "", "digest-pinned ateom image for the worker pool, e.g. ateom-gvisor built from the Substrate repo")
	cmd.Flags().StringVar(&cfg.snapshotsLocation, "snapshots-location", "", "object-storage location for suspend snapshots, e.g. gs://bucket/prefix/")
	cmd.Flags().StringVar(&cfg.pauseImage, "pause-image", defaultPauseImage, "digest-pinned pause image for the root sandbox container")
	cmd.Flags().StringVar(&cfg.workerPool, "workerpool", "", "WorkerPool name (defaults to <template>-workerpool)")
	cmd.Flags().Int32Var(&cfg.replicas, "replicas", 2, "number of pre-warmed worker pods")
	cmd.Flags().StringSliceVar(&cfg.guestdCommand, "guestd-command", []string{"/ko-app/substrate-guestd", "-workdir", "/workspace"}, "guestd container entrypoint")
	cmd.Flags().DurationVar(&cfg.waitForReady, "wait", 5*time.Minute, "how long to wait for the ActorTemplate to become Ready (0 to skip)")
	cmd.Flags().StringVar(&cfg.kubeconfig, "kubeconfig", "", "path to the kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	cmd.Flags().StringVar(&cfg.kubeContext, "kube-context", "", "kubeconfig context to use")
	cmd.MarkFlagRequired("guestd-image")
	cmd.MarkFlagRequired("ateom-image")
	cmd.MarkFlagRequired("snapshots-location")

	return cmd
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

	if cfg.waitForReady <= 0 {
		return nil
	}
	cmd.Printf("waiting up to %s for actortemplate %s/%s to become Ready...\n", cfg.waitForReady, cfg.namespace, cfg.template)
	if err := waitTemplateReady(ctx, ate, cfg.namespace, cfg.template, cfg.waitForReady); err != nil {
		return err
	}
	cmd.Printf("actortemplate %s/%s is Ready; create sandboxes with: sbcli create <id> --template %s --namespace %s\n",
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
				Location: cfg.snapshotsLocation,
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
