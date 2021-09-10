/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	capa "sigs.k8s.io/cluster-api-provider-aws/api/v1alpha3"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/giantswarm/capa-aws-cni-operator/pkg/awsclient"
	"github.com/giantswarm/capa-aws-cni-operator/pkg/cni"
	"github.com/giantswarm/capa-aws-cni-operator/pkg/key"
)

// AWSClusterReconciler reconciles a AWSMachinePool object
type AWSClusterReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	DefaultCNICIDR string
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsmachinepools/finalizers,verbs=update

func (r *AWSClusterReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var err error
	ctx := context.TODO()
	logger := r.Log.WithValues("namespace", req.Namespace, "awsCluster", req.Name)

	awsCluster := &capa.AWSCluster{}
	err = r.Get(ctx, req.NamespacedName, awsCluster)
	if k8serrors.IsNotFound(err) {
		// CR is gone, stop reconciling
		return ctrl.Result{
			Requeue: false,
		}, nil
	} else if err != nil {
		logger.Error(err, "failed fetching AWSCluster CR")
		return ctrl.Result{}, err
	}

	// check if CR got CAPI watch-filter label
	if !key.HasCapiWatchLabel(awsCluster.Labels) {
		logger.Info(fmt.Sprintf("AWSCluster do not have %s=%s label, ignoring CR", key.ClusterWatchFilterLabel, "capi"))
		// ignoring this CR
		return ctrl.Result{}, nil
	}

	clusterName := key.GetClusterIDFromLabels(awsCluster.ObjectMeta)

	logger = logger.WithValues("cluster", clusterName)

	if awsCluster.Spec.NetworkSpec.VPC.ID == "" {
		logger.Info("AWSCluster do not have vpc id set yet")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Minute * 2,
		}, nil
	}

	if len(awsCluster.Spec.NetworkSpec.Subnets.GetUniqueZones()) == 0 {
		logger.Info("AWSCluster do not have subnets set yet")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Minute * 2,
		}, nil
	}

	if len(awsCluster.Status.Network.SecurityGroups) == 0 {
		logger.Info("AWSCluster do not have security group set yet")
		return ctrl.Result{
			Requeue:      true,
			RequeueAfter: time.Minute * 2,
		}, nil
	}

	var awsClientGetter *awsclient.AwsClient
	{
		c := awsclient.AWSClientConfig{
			ClusterName: clusterName,
			CtrlClient:  r.Client,
			Log:         logger,
		}
		awsClientGetter, err = awsclient.New(c)
		if err != nil {
			logger.Error(err, "failed to generate awsClientGetter")
			return ctrl.Result{}, err
		}
	}

	awsClientSession, err := awsClientGetter.GetAWSClientSession(ctx)
	if err != nil {
		logger.Error(err, "Failed to get aws client session")
		return ctrl.Result{}, err
	}

	var clusterSecurityGroupIDs []string
	{
		for _, sg := range awsCluster.Status.Network.SecurityGroups {
			clusterSecurityGroupIDs = append(clusterSecurityGroupIDs, sg.ID)
		}

	}

	var cniService *cni.CNIService
	// config for the CNI service
	config := cni.CNIConfig{
		AWSSession:              awsClientSession,
		ClusterName:             clusterName,
		ClusterSecurityGroupIDs: clusterSecurityGroupIDs,
		CtrlClient:              nil,              // we only need wc k8s client for resource creation, we dont need it for deletion, when cluster is being deleted it might not be avaiable
		CNICIDR:                 r.DefaultCNICIDR, // we use default for now, but we might need a way how to get specify per cluster
		Log:                     logger,
		VPCAzList:               awsCluster.Spec.NetworkSpec.Subnets.GetUniqueZones(),
		VPCID:                   awsCluster.Spec.NetworkSpec.VPC.ID,
	}

	logger.Info("reconciling CR")
	// delete CNI resource
	if awsCluster.DeletionTimestamp != nil {
		cniService, err = cni.New(config)
		if err != nil {
			return ctrl.Result{}, err
		}
		err = cniService.Delete()
		if err != nil {
			return ctrl.Result{}, err
		}

		err = r.Get(ctx, req.NamespacedName, awsCluster)
		if err != nil {
			logger.Error(err, "failed to fetch latest AWSCluster")
			return ctrl.Result{}, err
		}
		if key.HasFinalizer(awsCluster.Finalizers) {
			controllerutil.RemoveFinalizer(awsCluster, key.FinalizerName)
			err = r.Update(ctx, awsCluster)
			if err != nil {
				logger.Error(err, "failed to remove finalizer on AWSCluster")
				return ctrl.Result{}, err
			}
		}
		// all resources were deleted, we dont have to reconcile anymore
		return ctrl.Result{
			Requeue: false,
		}, nil
	} else { // create CNI resource
		wcClient, err := key.GetWCK8sClient(ctx, r.Client, clusterName)
		if k8serrors.IsNotFound(err) {
			logger.Info("WC k8s api secrets are not ready yet")
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Minute * 2,
			}, nil
		} else if err != nil {
			return ctrl.Result{}, err
		}
		config.CtrlClient = wcClient
		cniService, err = cni.New(config)
		if err != nil {
			return ctrl.Result{}, err
		}

		// add finalizer to AWSCluster
		if !key.HasFinalizer(awsCluster.Finalizers) {
			controllerutil.AddFinalizer(awsCluster, key.FinalizerName)
			err = r.Update(ctx, awsCluster)
			if err != nil {
				logger.Error(err, "failed to add finalizer on AWSCluster")
				return ctrl.Result{}, err
			}
		}

		err = cniService.Reconcile()
		if strings.Contains(err.Error(), "aws-cni") {
			return ctrl.Result{
				Requeue:      true,
				RequeueAfter: time.Minute * 2,
			}, nil
		} else if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{
		Requeue:      true,
		RequeueAfter: time.Minute * 5,
	}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AWSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&capa.AWSCluster{}).
		Complete(r)
}
