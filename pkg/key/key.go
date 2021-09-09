package key

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	capa "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ClusterNameLabel        = "cluster.x-k8s.io/cluster-name"
	ClusterWatchFilterLabel = "cluster.x-k8s.io/watch-filter"

	FinalizerName = "capa-aws-cni-operator.finalizers.giantswarm.io"

	AWSCniOperatorOwnedTag = "capa-aws-cni-operator.giantswarm.io"
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
func GetWCK8sClient(ctx context.Context, ctrlClient client.Client, clusterName string) (client.Client, error) {
	awsCluster, err := GetAWSClusterByName(ctx, ctrlClient, clusterName)
	if err != nil {
		return nil, err
	}

	apiEndpoint := fmt.Sprintf("%s:%d", awsCluster.Spec.ControlPlaneEndpoint.Host, awsCluster.Spec.ControlPlaneEndpoint.Port)

	var secret corev1.Secret
	{
		err = ctrlClient.Get(ctx, client.ObjectKey{
			Name:      fmt.Sprintf("%s-ca", GetClusterIDFromLabels(awsCluster.ObjectMeta)),
			Namespace: awsCluster.Namespace,
		},
			&secret)

		if err != nil {
			return nil, err
		}
	}

	conf := &rest.Config{
		Host: apiEndpoint,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   secret.Data["tls.crt"],
			CertData: secret.Data["tls.crt"],
			KeyData:  secret.Data["tls.key"],
		},
	}

	wcClient, err := client.New(conf, client.Options{})
	if err != nil {
		return nil, err
	}

	// check if k8s api is already available
	var nsList corev1.NamespaceList
	err = wcClient.List(ctx, &nsList)
	if err != nil {
		// wc k8s api si not ready yet
		return nil, err
	}

	return wcClient, nil
}
