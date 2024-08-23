package main

import (
	"context"
	"flag"
	"fmt"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"k8s.io/utils/strings/slices"
	"path/filepath"
	"strings"
	"time"
)

const (
	DatabaseMatch          = "database"
	ValidNameMaxLength     = 255
	WaitForRestartTimeout  = time.Duration(300 * time.Second)
	ConfigRestartInterval  = 2
	ConfigNameSuffixLength = 5
)

type kubeClient struct {
	clientSet *kubernetes.Clientset
}

func main() {
	var k kubeClient
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	k.clientSet, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	// It seems I cannot filter the lookup and must retrieve all PODs in the cluster as there are no
	// known labels to select, and `database` can be anywhere in the name
	// https://github.com/kubernetes/kubernetes/issues/72196
	// https://github.com/kubernetes/kubernetes/issues/109400
	pods, err := k.clientSet.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	type podError struct {
		name         string
		restartError error
	}

	// instantiate vars for holding a list of errors and already restarted higher level resources
	var allErrs []podError
	var restarted []string

	// We'll use the RestartedAt annotation for higher level resources, and then duplicate a Pod
	// spec with a new randomized name suffix
	// https://github.com/kubernetes/kubectl/blob/master/pkg/cmd/rollout/rollout.go
	// https://kubernetes.io/docs/reference/labels-annotations-taints/#kubectl-k8s-io-restart-at
	for _, pod := range pods.Items {
		// skip anny pods without database in the name
		if !strings.Contains(pod.Name, DatabaseMatch) {
			continue
		}

		fmt.Printf("executing graceful restart on pod: %s\n", pod.Name)
		err := k.restartResourceFromPod(context.TODO(), &restarted, pod)
		if err != nil {
			allErrs = append(allErrs, podError{pod.Name, err})
			continue
		}
	}

	if len(allErrs) > 0 {
		fmt.Println(allErrs)
	}

	fmt.Printf("finished restarting %d resources: %s\n", len(restarted), restarted)
}

func getResourceType(name string) string {
	var resourceType string
	switch name {
	case "ReplicaSet":
		resourceType = "ReplicaSet"
	case "Deployment":
		resourceType = "Deployment"
	case "StatefulSet":
		resourceType = "StatefulSet"
	case "DaemonSet":
		resourceType = "DaemonSet"
	case "":
		resourceType = "Pod"
	default:
		resourceType = "unsupported"
	}
	return resourceType
}

func (c *kubeClient) restartDeployment(ctx context.Context, name, namespace string) error {
	deploy, err := c.clientSet.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if deploy.Spec.Template.ObjectMeta.Annotations == nil {
		deploy.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.ObjectMeta.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	_, err = c.clientSet.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	return err
}

func (c *kubeClient) restartDaemonSet(ctx context.Context, name, namespace string) error {
	ds, err := c.clientSet.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if ds.Spec.Template.ObjectMeta.Annotations == nil {
		ds.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}
	ds.Spec.Template.ObjectMeta.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	_, err = c.clientSet.AppsV1().DaemonSets(namespace).Update(ctx, ds, metav1.UpdateOptions{})
	return err
}

func (c *kubeClient) restartStatefulSet(ctx context.Context, name, namespace string) error {
	sts, err := c.clientSet.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if sts.Spec.Template.ObjectMeta.Annotations == nil {
		sts.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}
	sts.Spec.Template.ObjectMeta.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	_, err = c.clientSet.AppsV1().StatefulSets(namespace).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

func (c *kubeClient) restartResourceFromPod(ctx context.Context, restarted *[]string, pod v1.Pod) error {
	// retrieve owner references to identify supported restart resources
	ownerRefs := pod.OwnerReferences
	var resourceType string
	if ownerRefs == nil || len(ownerRefs) == 0 {
		resourceType = "Pod"
		if err := c.restartResource(ctx, resourceType, "", pod); err != nil {
			return err
		}
		*restarted = append(*restarted, pod.Name)
	}

	for _, ownerRef := range ownerRefs {
		resourceType = getResourceType(ownerRef.Kind)
		if resourceType == "unknown" {
			fmt.Printf("skipping restart unknown resource type for pod: %s\n", pod.Name)
			continue
		}

		match := fmt.Sprintf("%s|%s|%s", ownerRef.Name, ownerRef.Kind, pod.Namespace)
		// ensure we don't keep restarting the same higher level resource
		if resourceType != "Pod" {
			if slices.Contains(*restarted, match) {
				fmt.Printf("skipping already restarted resource: %s\n", match)
				continue
			}
			if err := c.restartResource(ctx, resourceType, ownerRef.Name, pod); err != nil {
				return err
			}
			*restarted = append(*restarted, match)
		}
	}

	return nil
}

func (c *kubeClient) restartReplicaSet(ctx context.Context, name, namespace string) error {
	rs, err := c.clientSet.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var resourceType string
	for _, ownerRef := range rs.OwnerReferences {
		resourceType = getResourceType(ownerRef.Kind)
		if resourceType == "Deployment" {
			return c.restartDeployment(ctx, ownerRef.Name, namespace)
		}
	}
	return nil
}

func (c *kubeClient) restartResource(ctx context.Context, resourceType, name string, pod v1.Pod) error {
	switch resourceType {
	case "ReplicaSet":
		return c.restartReplicaSet(ctx, name, pod.Namespace)
	case "Deployment":
		return c.restartDeployment(ctx, name, pod.Namespace)
	case "StatefulSet":
		return c.restartStatefulSet(ctx, name, pod.Namespace)
	case "DaemonSet":
		return c.restartDaemonSet(ctx, name, pod.Namespace)
	case "Pod":
		return c.restartPod(ctx, pod)
	}

	return nil
}

func (c *kubeClient) restartPod(ctx context.Context, pod v1.Pod) error {
	suffix := rand.String(ConfigNameSuffixLength - 1)
	var newPodName string
	if len(pod.Name) > ValidNameMaxLength {
		newPodName = newSuffixPodName(pod.Name[:len(pod.Name)-ConfigNameSuffixLength], suffix)
	} else {
		newPodName = newSuffixPodName(pod.Name, suffix)
	}

	newPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newPodName,
			Namespace: pod.Namespace,
			Labels:    pod.Labels,
		},
		Spec: pod.Spec,
	}

	instance, err := c.clientSet.CoreV1().Pods(newPod.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	start := time.Now()
	for {
		if time.Since(start) > WaitForRestartTimeout {
			fmt.Printf("timed out waiting for Pod to restart: %s in namespace: %s\n", instance.Name, instance.Namespace)
			break
		}
		if c.isPodRunning(ctx, instance.Name, instance.Namespace) {
			fmt.Printf("replacing pod: %s with %s in namespace %s\n", pod.Name, instance.Name, instance.Namespace)
			return c.deletePod(ctx, pod.Name, pod.Namespace)
		}
		time.Sleep(ConfigRestartInterval * time.Second)
	}

	return nil
}

func (c *kubeClient) deletePod(ctx context.Context, name, namespace string) error {
	return c.clientSet.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *kubeClient) isPodRunning(ctx context.Context, name, namespace string) bool {
	pod, err := c.clientSet.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}

	switch pod.Status.Phase {
	case v1.PodRunning:
		return true
	case v1.PodSucceeded, v1.PodFailed:
		return false
	}

	return false
}

func newSuffixPodName(name, suffix string) string {
	return fmt.Sprintf("%s-%s", name, suffix)
}
