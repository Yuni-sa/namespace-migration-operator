package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	_ "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfigPath   string
	reconcileTimeout time.Duration
)

func init() {
	flag.StringVar(&kubeconfigPath, "kubeconfig", "", "Path to the kubeconfig file")
	flag.DurationVar(&reconcileTimeout, "reconcile-timeout", 5*time.Minute, "Reconciliation timeout duration")
}

func main() {
	flag.Parse()

	if kubeconfigPath == "" {
		log.Fatal("kubeconfig flag is required")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		log.Fatalf("Failed to build config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
	}

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-stopCh
		cancel()
	}()

	for {
		select {
		case <-ctx.Done():
			log.Println("Reconciliation stopped")
			return
		case <-time.After(reconcileTimeout):
			log.Println("Reconciliation timeout reached")
			return
		default:
			err := reconcileDeployment(ctx, clientset)
			if err != nil {
				log.Printf("Failed to reconcile Deployment: %v\n", err)
			}
			time.Sleep(10 * time.Second)
		}
	}
}

func reconcileDeployment(ctx context.Context, clientset *kubernetes.Clientset) error {
	deploymentsClient := clientset.AppsV1().Deployments(v1.NamespaceAll)
	selector := labels.Set{"app": "example-app"}.AsSelector()

	deploymentList, err := deploymentsClient.List(ctx, v1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return fmt.Errorf("Failed to retrieve Deployments: %v", err)
	}

	for _, deployment := range deploymentList.Items {
		annotations := deployment.GetAnnotations()

		if annotations["ns"] == "test" {
			err = moveDeployment(ctx, clientset, &deployment, "test2")
			if err != nil {
				log.Printf("Failed to move Deployment %q to test2: %v\n", deployment.Name, err)
			}
		}
	}

	return nil
}

func moveDeployment(ctx context.Context, clientset *kubernetes.Clientset, deployment *appsv1.Deployment, targetNamespace string) error {
	sourceNamespace := deployment.Namespace
	deploymentName := deployment.Name

	log.Printf("Moving Deployment %q from %q to %q\n", deploymentName, sourceNamespace, targetNamespace)

	// Get the current deployment in the source namespace
	sourceDeployment, err := clientset.AppsV1().Deployments(sourceNamespace).Get(ctx, deploymentName, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Failed to retrieve Deployment %q in namespace %q: %v", deploymentName, sourceNamespace, err)
	}

	// Update the deployment's namespace
	sourceDeployment.Namespace = targetNamespace
	sourceDeployment.ResourceVersion = "" // Clear the ResourceVersion field
	_, err = clientset.AppsV1().Deployments(targetNamespace).Create(ctx, sourceDeployment, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("Failed to create Deployment %q in namespace %q: %v", deploymentName, targetNamespace, err)
	}

	// Delete the original deployment in the source namespace
	err = clientset.AppsV1().Deployments(sourceNamespace).Delete(ctx, deploymentName, v1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("Failed to delete Deployment %q from namespace %q: %v", deploymentName, sourceNamespace, err)
	}

	// Update the service's namespace
	err = moveService(ctx, clientset, deployment, targetNamespace)
	if err != nil {
		return fmt.Errorf("Failed to move Service associated with Deployment %q: %v", deploymentName, err)
	}

	return nil
}

func moveService(ctx context.Context, clientset *kubernetes.Clientset, deployment *appsv1.Deployment, targetNamespace string) error {
	sourceNamespace := deployment.Namespace
	serviceName := deployment.Name

	log.Printf("Moving Service %q from %q to %q\n", serviceName, sourceNamespace, targetNamespace)

	// Get the current service in the source namespace
	sourceService, err := clientset.CoreV1().Services(sourceNamespace).Get(ctx, serviceName, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Failed to retrieve Service %q in namespace %q: %v", serviceName, sourceNamespace, err)
	}

	// Update the service's namespace
	sourceService.Namespace = targetNamespace
	_, err = clientset.CoreV1().Services(targetNamespace).Create(ctx, sourceService, v1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("Failed to create Service %q in namespace %q: %v", serviceName, targetNamespace, err)
	}

	// Delete the original service in the source namespace
	err = clientset.CoreV1().Services(sourceNamespace).Delete(ctx, serviceName, v1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("Failed to delete Service %q from namespace %q: %v", serviceName, sourceNamespace, err)
	}

	return nil
}
