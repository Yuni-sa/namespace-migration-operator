package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/metrics/pkg/client/clientset/versioned"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

var (
	kubeconfigPath   string
	sourceNamespace  string
	targetNamespace  string
	cpuThreshold     int64
	reconcileTimeout time.Duration
)

func init() {
	flag.StringVar(&kubeconfigPath, "kubeconfig", "", "Path to the kubeconfig file")
	flag.StringVar(&sourceNamespace, "source-namespace", "", "Source namespace")
	flag.StringVar(&targetNamespace, "target-namespace", "", "Target namespace")
	flag.Int64Var(&cpuThreshold, "cpu-threshold", 90, "CPU threshold percentage")
	flag.DurationVar(&reconcileTimeout, "reconcile-timeout", 5*time.Minute, "Reconciliation timeout duration")
}

func main() {
	flag.Parse()

	if kubeconfigPath == "" || sourceNamespace == "" || targetNamespace == "" {
		log.Fatal("kubeconfig, source-namespace, and target-namespace flags are required")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		log.Fatalf("Failed to build config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	metricsClient, err := versioned.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create metrics client: %v", err)
	}

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-stopCh
		cancel()
	}()

	err = reconcileDeployment(ctx, clientset, metricsClient)
	if err != nil {
		log.Fatalf("Failed to reconcile Deployment: %v", err)
	}

	log.Println("Reconciliation completed!")
}

func reconcileDeployment(ctx context.Context, clientset *kubernetes.Clientset, metricsClient *versioned.Clientset) error {
	deploymentsClient := clientset.AppsV1().Deployments(sourceNamespace)
	deploymentName := "example-deployment"

	deployment, err := deploymentsClient.Get(ctx, deploymentName, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("Deployment %q not found", deploymentName)
		}
		return err
	}

	log.Printf("Reconciling Deployment %q\n", deploymentName)

	podList, err := clientset.CoreV1().Pods(sourceNamespace).List(ctx, v1.ListOptions{LabelSelector: labelsToSelector(deployment.Spec.Selector)})
	if err != nil {
		return fmt.Errorf("Failed to retrieve Pods for Deployment %q: %v", deploymentName, err)
	}

	cpuUsagePercent, err := getCPUUsagePercent(ctx, metricsClient, deployment.Namespace, podList)
	if err != nil {
		return fmt.Errorf("Failed to retrieve CPU usage for Deployment %q: %v", deploymentName, err)
	}

	if cpuUsagePercent > cpuThreshold {
		err = moveDeployment(ctx, clientset)
		if err != nil {
			return fmt.Errorf("Failed to move Deployment %q: %v", deploymentName, err)
		}

		err = moveService(ctx, clientset)
		if err != nil {
			return fmt.Errorf("Failed to move Service for Deployment %q: %v", deploymentName, err)
		}

		log.Printf("Deployment %q and Service moved to %q\n", deploymentName, targetNamespace)
	} else {
		log.Printf("Deployment %q CPU usage (%v%%) is below the threshold (%v%%)\n", deploymentName, cpuUsagePercent, cpuThreshold)
	}

	return nil
}

func getCPUUsagePercent(ctx context.Context, metricsClient *versioned.Clientset, namespace string, podList *corev1.PodList) (int64, error) {
	totalCpuUsage := int64(0)
	totalCpuLimit := int64(0)

	for _, pod := range podList.Items {
		podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(namespace).Get(ctx, pod.Name, v1.GetOptions{})
		if err != nil {
			return 0, fmt.Errorf("Failed to retrieve metrics for Pod %q: %v", pod.Name, err)
		}

		for _, container := range podMetrics.Containers {
			cpuUsage := container.Usage[corev1.ResourceCPU]
			cpuLimit := container.Usage[corev1.ResourceLimitsCPU]

			cpuUsageMillis := cpuUsage.MilliValue()
			cpuLimitMillis := cpuLimit.MilliValue()

			totalCpuUsage += cpuUsageMillis
			totalCpuLimit += cpuLimitMillis
		}
	}

	if totalCpuLimit == 0 {
		return 0, nil
	}

	cpuUsagePercent := (totalCpuUsage * 100) / totalCpuLimit
	return cpuUsagePercent, nil
}

func labelsToSelector(labels *v1.LabelSelector) string {
	var labelSelectors []string
	for key, value := range labels.MatchLabels {
		labelSelectors = append(labelSelectors, fmt.Sprintf("%s=%s", key, value))
	}
	return strings.Join(labelSelectors, ",")
}

func moveDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	deploymentsClient := clientset.AppsV1().Deployments(sourceNamespace)
	deploymentName := "example-deployment"

	deployment, err := deploymentsClient.Get(ctx, deploymentName, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("Deployment %q not found", deploymentName)
		}
		return err
	}

	deployment.Namespace = targetNamespace
	deployment.ResourceVersion = ""
	_, err = deploymentsClient.Create(ctx, deployment, v1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			_, err = deploymentsClient.Update(ctx, deployment, v1.UpdateOptions{})
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	deletePropagation := v1.DeletePropagationForeground
	deleteOptions := &v1.DeleteOptions{PropagationPolicy: &deletePropagation}
	err = deploymentsClient.Delete(ctx, deploymentName, *deleteOptions)
	if err != nil {
		return err
	}

	log.Printf("Deployment %q moved from %q to %q\n", deploymentName, sourceNamespace, targetNamespace)
	return nil
}

func moveService(ctx context.Context, clientset *kubernetes.Clientset) error {
	servicesClient := clientset.CoreV1().Services(sourceNamespace)
	serviceName := "example-service"

	service, err := servicesClient.Get(ctx, serviceName, v1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("Service %q not found", serviceName)
		}
		return err
	}

	service.Namespace = targetNamespace
	service.ResourceVersion = ""
	_, err = servicesClient.Create(ctx, service, v1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			_, err = servicesClient.Update(ctx, service, v1.UpdateOptions{})
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	deleteOptions := &v1.DeleteOptions{}
	err = servicesClient.Delete(ctx, serviceName, *deleteOptions)
	if err != nil {
		return err
	}

	log.Printf("Service %q moved from %q to %q\n", serviceName, sourceNamespace, targetNamespace)
	return nil
}
