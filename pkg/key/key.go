package key

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"

	eni "github.com/aws/amazon-vpc-cni-k8s/pkg/apis/crd/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	capa "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ClusterNameLabel        = "cluster.x-k8s.io/cluster-name"
	ClusterWatchFilterLabel = "cluster.x-k8s.io/watch-filter"

	FinalizerName = "capa-aws-cni-operator.finalizers.giantswarm.io"

	AWSCniOperatorOwnedTag = "capa-aws-cni-operator.giantswarm.io"

	CNINodeSecurityGroupName = "node"
)

func GetClusterIDFromLabels(t metav1.ObjectMeta) string {
	return t.GetLabels()[ClusterNameLabel]
}

func GetAWSClusterByName(ctx context.Context, ctrlClient client.Client, clusterName string) (*capa.AWSCluster, error) {
	awsClusterList := &capa.AWSClusterList{}

	if err := ctrlClient.List(ctx,
		awsClusterList,
		client.MatchingLabels{ClusterNameLabel: clusterName},
	); err != nil {
		return nil, err
	}

	if len(awsClusterList.Items) != 1 {
		return nil, fmt.Errorf("expected 1 AWSCluster but found %d", len(awsClusterList.Items))
	}

	return &awsClusterList.Items[0], nil
}

func HasCapiWatchLabel(labels map[string]string) bool {
	value, ok := labels[ClusterWatchFilterLabel]
	if ok {
		if value == "capi" {
			return true
		}
	}
	return false
}

// GetWCK8sClient will return workload cluster k8s controller-runtime client
func GetWCK8sClient(ctx context.Context, ctrlClient client.Client, clusterName string, clusterNamespace string) (client.Client, error) {
	var err error

	if _, err := os.Stat(tempKubeconfigFileName(clusterName)); err == nil {
		// kubeconfig file already exists, no need to fetch and write again

	} else if os.IsNotExist(err) {
		// kubeconfig dont exists we need to fetch it and write to file
		var secret corev1.Secret
		{
			err = ctrlClient.Get(ctx, client.ObjectKey{
				Name:      fmt.Sprintf("%s-kubeconfig", clusterName),
				Namespace: clusterNamespace,
			},
				&secret)

			if err != nil {
				return nil, err
			}
		}
		err = ioutil.WriteFile(tempKubeconfigFileName(clusterName), secret.Data["value"], 0600)
		if err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}

	config, err := clientcmd.BuildConfigFromFlags("", tempKubeconfigFileName(clusterName))
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	_ = eni.AddToScheme(scheme)

	wcClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}

	return wcClient, nil
}

func HasFinalizer(finalizers []string) bool {
	for _, f := range finalizers {
		if f == FinalizerName {
			return true
		}
	}
	return false
}

func tempKubeconfigFileName(clusterName string) string {
	return fmt.Sprintf("/tmp/kubeconfig-%s", clusterName)
}
