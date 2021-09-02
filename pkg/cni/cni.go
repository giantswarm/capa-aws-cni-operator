package cni

import (
	"errors"
	"net"

	awsclient "github.com/aws/aws-sdk-go/aws/client"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CNIConfig struct {
	AWSSession awsclient.ConfigProvider
	CtrlClient client.Client
	CNICIDR    string
}

type CNIService struct {
	awsSession awsclient.ConfigProvider
	ctrlClient client.Client
	cniCIDR    string
}

func New(c CNIConfig) (*CNIService, error) {
	if c.AWSSession == nil {
		return nil, errors.New("failed to generate new cni service from nil AWSSession")
	}

	if c.CtrlClient == nil {
		return nil, errors.New("failed to generate new cni service from nil CtrlClient")
	}

	_, _, err := net.ParseCIDR(c.CNICIDR)
	if err != nil {
		return nil, err
	}

	s := &CNIService{
		awsSession: c.AWSSession,
		ctrlClient: c.CtrlClient,
		cniCIDR:    c.CNICIDR,
	}
	return s, nil
}

func (c *CNIService) Reconcile() error {

	return nil
}

func (c *CNIService) Delete() error {

	return nil
}
