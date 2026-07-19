package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	atefake "github.com/agent-substrate/substrate/pkg/client/clientset/versioned/fake"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func testDeployConfig() deployConfig {
	return deployConfig{
		namespace:       "substrate-sandbox",
		template:        "sandbox",
		workerPool:      "sandbox-workerpool",
		guestdImage:     "example.com/guestd@sha256:aaaa",
		ateomImage:      "example.com/ateom@sha256:bbbb",
		apiImage:        "example.com/api@sha256:cccc",
		pauseImage:      defaultPauseImage,
		snapshotsBucket: "gs://bucket/substrate-sandbox/",
		replicas:        3,
		apiReplicas:     1,
		guestdCommand:   []string{"/ko-app/substrate-guestd", "-workdir", "/workspace"},
		poolLabels:      map[string]string{"workload": "sandbox"},
	}
}

func quietCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	return cmd
}

func TestResolveImages(t *testing.T) {
	cfg := testDeployConfig()
	if err := cfg.resolveImages(); err != nil {
		t.Errorf("resolveImages with both images set: %v", err)
	}
	cfg.ateomImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing image: want error, got nil")
	}
	cfg = testDeployConfig()
	cfg.apiImage = ""
	if err := cfg.resolveImages(); err == nil {
		t.Error("resolveImages with a missing api image: want error, got nil")
	}
}

func TestRunDeployCreatesResources(t *testing.T) {
	kube := kubefake.NewClientset()
	ate := atefake.NewSimpleClientset()
	cfg := testDeployConfig()
	cfg.waitForReady = 0 // the fake has no controller to flip Ready

	if err := runDeploy(context.Background(), quietCommand(), kube, ate, cfg); err != nil {
		t.Fatalf("runDeploy: %v", err)
	}

	if _, err := kube.CoreV1().Namespaces().Get(context.Background(), "substrate-sandbox", metav1.GetOptions{}); err != nil {
		t.Errorf("namespace not created: %v", err)
	}

	pool, err := ate.ApiV1alpha1().WorkerPools("substrate-sandbox").Get(context.Background(), "sandbox-workerpool", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("workerpool not created: %v", err)
	}
	if pool.Spec.Replicas != 3 || pool.Spec.AteomImage != cfg.ateomImage {
		t.Errorf("workerpool spec = %+v, want replicas 3 and ateom image %q", pool.Spec, cfg.ateomImage)
	}
	if pool.Labels["workload"] != "sandbox" {
		t.Errorf("workerpool labels = %v, want workload=sandbox", pool.Labels)
	}

	template, err := ate.ApiV1alpha1().ActorTemplates("substrate-sandbox").Get(context.Background(), "sandbox", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("actortemplate not created: %v", err)
	}
	if len(template.Spec.Containers) != 1 || template.Spec.Containers[0].Image != cfg.guestdImage {
		t.Errorf("template containers = %+v, want one guestd container with image %q",
			template.Spec.Containers, cfg.guestdImage)
	}
	if template.Spec.SnapshotsConfig.Location != cfg.snapshotsBucket {
		t.Errorf("snapshots location = %q, want %q", template.Spec.SnapshotsConfig.Location, cfg.snapshotsBucket)
	}
	if got := template.Spec.WorkerSelector.MatchLabels["workload"]; got != "sandbox" {
		t.Errorf("worker selector = %v, want workload=sandbox", template.Spec.WorkerSelector)
	}
	readyz := template.Spec.Containers[0].Readyz
	if readyz == nil || readyz.HTTPGet == nil || readyz.HTTPGet.Path != "/healthz" {
		t.Errorf("readyz = %+v, want HTTP GET /healthz", readyz)
	}

	deployment, err := kube.AppsV1().Deployments("substrate-sandbox").Get(context.Background(), apiName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("api deployment not created: %v", err)
	}
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Image != cfg.apiImage {
		t.Errorf("api containers = %+v, want one container with image %q", containers, cfg.apiImage)
	}
	if *deployment.Spec.Replicas != 1 {
		t.Errorf("api replicas = %d, want 1", *deployment.Spec.Replicas)
	}

	service, err := kube.CoreV1().Services("substrate-sandbox").Get(context.Background(), apiName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("api service not created: %v", err)
	}
	if service.Spec.Selector["app"] != apiName {
		t.Errorf("api service selector = %v, want app=%s", service.Spec.Selector, apiName)
	}
	if len(service.Spec.Ports) != 1 || service.Spec.Ports[0].Port != 7777 || service.Spec.Ports[0].TargetPort.IntValue() != 7777 {
		t.Errorf("api service ports = %+v, want 7777 -> 7777", service.Spec.Ports)
	}
}

func TestRunDeployIsIdempotent(t *testing.T) {
	kube := kubefake.NewClientset()
	ate := atefake.NewSimpleClientset()
	cfg := testDeployConfig()
	cfg.waitForReady = 0

	if err := runDeploy(context.Background(), quietCommand(), kube, ate, cfg); err != nil {
		t.Fatalf("first deploy: %v", err)
	}

	// A second deploy with changed settings updates in place.
	cfg.replicas = 5
	cfg.guestdImage = "example.com/guestd@sha256:cccc"
	if err := runDeploy(context.Background(), quietCommand(), kube, ate, cfg); err != nil {
		t.Fatalf("second deploy: %v", err)
	}

	pool, err := ate.ApiV1alpha1().WorkerPools("substrate-sandbox").Get(context.Background(), "sandbox-workerpool", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pool.Spec.Replicas != 5 {
		t.Errorf("replicas after re-deploy = %d, want 5", pool.Spec.Replicas)
	}
	template, err := ate.ApiV1alpha1().ActorTemplates("substrate-sandbox").Get(context.Background(), "sandbox", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if template.Spec.Containers[0].Image != "example.com/guestd@sha256:cccc" {
		t.Errorf("guestd image after re-deploy = %q, want the updated digest", template.Spec.Containers[0].Image)
	}
}

func TestRunDeployWaitsForReady(t *testing.T) {
	kube := kubefake.NewClientset()
	ate := atefake.NewSimpleClientset()
	cfg := testDeployConfig()
	cfg.waitForReady = 5 * time.Second

	// Flip the template to Ready shortly after deploy creates it,
	// simulating the Substrate controller.
	go func() {
		for {
			template, err := ate.ApiV1alpha1().ActorTemplates(cfg.namespace).Get(context.Background(), cfg.template, metav1.GetOptions{})
			if err == nil {
				template.Status.Phase = atev1alpha1.PhaseReady
				ate.ApiV1alpha1().ActorTemplates(cfg.namespace).Update(context.Background(), template, metav1.UpdateOptions{})
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	if err := runDeploy(context.Background(), quietCommand(), kube, ate, cfg); err != nil {
		t.Fatalf("runDeploy with wait: %v", err)
	}
}
