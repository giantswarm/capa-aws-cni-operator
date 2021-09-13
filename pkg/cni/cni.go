package cni

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/apis/crd/v1alpha1"
	"github.com/aws/aws-sdk-go/aws"
	awsclient "github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/giantswarm/ipam"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/giantswarm/capa-aws-cni-operator/pkg/key"
)

type CNISubnet struct {
	AZ       string
	SubnetID string
}

type CNIConfig struct {
	AWSSession              awsclient.ConfigProvider
	ClusterName             string
	ClusterSecurityGroupIDs []string
	CtrlClient              client.Client
	CNICIDR                 string
	Log                     logr.Logger
	VPCAzList               []string
	VPCID                   string
}

type CNIService struct {
	awsSession              awsclient.ConfigProvider
	clusterName             string
	clusterSecurityGroupIDs []string
	ctrlClient              client.Client
	cniCIDR                 string
	log                     logr.Logger
	vpcAzList               []string
	vpcID                   string
}

func New(c CNIConfig) (*CNIService, error) {
	if c.AWSSession == nil {
		return nil, errors.New("failed to generate new cni service from nil AWSSession")
	}

	if c.ClusterName == "" {
		return nil, errors.New("failed to generate new cni service from empty ClusterName")
	}

	if len(c.ClusterSecurityGroupIDs) == 0 {
		return nil, errors.New("failed to generate new cni service from empty ClusterSecurityGroupIDs")
	}

	_, _, err := net.ParseCIDR(c.CNICIDR)
	if err != nil {
		return nil, err
	}

	if c.Log == nil {
		return nil, errors.New("failed to generate new cni service from nil logger")
	}

	if len(c.VPCAzList) == 0 {
		return nil, errors.New("failed to generate new cni service from empty VPCAzList")
	}

	if c.VPCID == "" {
		return nil, errors.New("failed to generate new cni service from empty VPCID")
	}

	s := &CNIService{
		awsSession:              c.AWSSession,
		clusterName:             c.ClusterName,
		clusterSecurityGroupIDs: c.ClusterSecurityGroupIDs,
		ctrlClient:              c.CtrlClient,
		cniCIDR:                 c.CNICIDR,
		log:                     c.Log,
		vpcAzList:               c.VPCAzList,
		vpcID:                   c.VPCID,
	}
	return s, nil
}

func (c *CNIService) Reconcile() error {
	ec2Client := ec2.New(c.awsSession)

	// associate CNI  CIDR to the cluster VPC
	err := c.associateVPCCidrBlock(ec2Client)
	if err != nil {
		return err
	}

	// create subnets for CNI in each AZ
	cniSubnets, err := c.createSubnets(ec2Client)
	if err != nil {
		return err
	}

	// create cni security group
	//securityGroupID, err := c.createSecurityGroup(ec2Client)
	//if err != nil {
	//	return err
	//}

	// apply eni configs to WC k8s
	err = c.applyENIConfigs(cniSubnets, c.clusterSecurityGroupIDs[0])
	if err != nil {
		return err
	}

	return nil
}

// associateVPCCidrBlock will add CNI subnet to the cluster VPC
func (c *CNIService) associateVPCCidrBlock(ec2Client *ec2.EC2) error {
	inputDescribe := &ec2.DescribeVpcsInput{VpcIds: aws.StringSlice([]string{c.vpcID})}

	o, err := ec2Client.DescribeVpcs(inputDescribe)
	if err != nil {
		c.log.Error(err, "failed to describe VPC")
		return err
	}
	alreadyAssociated := false

	// check if the cidr is already associated
	for _, a := range o.Vpcs[0].CidrBlockAssociationSet {
		if *a.CidrBlock == c.cniCIDR {
			alreadyAssociated = true
			break
		}
	}

	if alreadyAssociated {
		c.log.Info(fmt.Sprintf("CNI CIDR block %s is already associated with vpc", c.cniCIDR))
	} else {
		i := &ec2.AssociateVpcCidrBlockInput{
			VpcId:     aws.String(c.vpcID),
			CidrBlock: aws.String(c.cniCIDR),
		}
		_, err := ec2Client.AssociateVpcCidrBlock(i)
		if err != nil {
			c.log.Error(err, fmt.Sprintf("failed to associate VPC cidr block '%s'", c.cniCIDR))
			return err
		}
		c.log.Info(fmt.Sprintf("associated new CNI CIDR block %s with vpc", c.cniCIDR))
	}

	return nil
}

// createSubnets will create subnets for aws cni for each AZ that is used in the cluster
func (c *CNIService) createSubnets(ec2Client *ec2.EC2) ([]CNISubnet, error) {
	// subnets
	var cniSubnets []CNISubnet
	_, cniNetwork, _ := net.ParseCIDR(c.cniCIDR)
	cniSubnetRanges, err := ipam.Split(*cniNetwork, uint(len(c.vpcAzList)))
	if err != nil {
		c.log.Error(err, fmt.Sprintf("failed to split cni network %s into %d parts", cniNetwork.String(), len(c.vpcAzList)))
		return nil, err
	}

	// create AWS CNI subnet for each AZ
	for i, az := range c.vpcAzList {
		// check if the subnet already exists
		describeInput := &ec2.DescribeSubnetsInput{
			Filters: []*ec2.Filter{
				{
					Name:   aws.String("tag:Name"),
					Values: aws.StringSlice([]string{subnetName(c.clusterName, az)}),
				},
				{
					Name:   aws.String(fmt.Sprintf("tag:%s", key.AWSCniOperatorOwnedTag)),
					Values: aws.StringSlice([]string{"owned"}),
				},
				{
					Name:   aws.String("vpc-id"),
					Values: aws.StringSlice([]string{c.vpcID}),
				},
			},
		}
		o, err := ec2Client.DescribeSubnets(describeInput)

		if err == nil && len(o.Subnets) == 1 {
			// subnet already exist, just save the ID
			cniSubnets = append(cniSubnets, CNISubnet{
				SubnetID: *o.Subnets[0].SubnetId,
				AZ:       az,
			})
			c.log.Info(fmt.Sprintf("cni subnet %s already created with id %s", subnetName(c.clusterName, az), *o.Subnets[0].SubnetId))

		} else if err == nil {
			// create subnet
			createInput := &ec2.CreateSubnetInput{
				VpcId:            aws.String(c.vpcID),
				AvailabilityZone: aws.String(az),
				CidrBlock:        aws.String(cniSubnetRanges[i].String()),
				TagSpecifications: []*ec2.TagSpecification{
					{
						Tags: []*ec2.Tag{
							{
								Key:   aws.String("Name"),
								Value: aws.String(subnetName(c.clusterName, az)),
							},
							{
								Key:   aws.String(key.AWSCniOperatorOwnedTag),
								Value: aws.String("owned"),
							},
						},
						ResourceType: aws.String("subnet"),
					},
				},
			}
			o, err := ec2Client.CreateSubnet(createInput)
			if err != nil {
				c.log.Error(err, fmt.Sprintf("failed to create aws cni subnet for AZ %s with subnet range  %s", az, cniSubnetRanges[i].String()))
				return nil, err
			}
			cniSubnets = append(cniSubnets, CNISubnet{
				SubnetID: *o.Subnet.SubnetId,
				AZ:       az,
			})
			c.log.Info(fmt.Sprintf("created cni subnet %s with id %s", subnetName(c.clusterName, az), *o.Subnet.SubnetId))
		} else {
			c.log.Error(err, fmt.Sprintf("failed to describe subnet %s", subnetName(c.clusterName, az)))
			return nil, err
		}
	}
	return cniSubnets, nil
}

// createSecurityGroup will create security group for aws cni
// and apply security ingress rules to allow all traffic for all security groups in the cluster
/*
func (c *CNIService) createSecurityGroup(ec2Client *ec2.EC2) (string, error) {
	var securityGroupID string

	// first we check if the security group already exist
	i := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: aws.StringSlice([]string{securityGroupName(c.clusterName)}),
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:%s", key.AWSCniOperatorOwnedTag)),
				Values: aws.StringSlice([]string{"owned"}),
			},
			{
				Name:   aws.String("vpc-id"),
				Values: aws.StringSlice([]string{c.vpcID}),
			},
		},
	}
	o, err := ec2Client.DescribeSecurityGroups(i)

	if err == nil && len(o.SecurityGroups) == 1 {
		// group already exists just save the ID
		securityGroupID = *o.SecurityGroups[0].GroupId
		c.log.Info(fmt.Sprintf("cni security group %s already created with id %s", securityGroupName(c.clusterName), securityGroupID))
	} else if err == nil {
		// security group does not exist, create a new one
		i := &ec2.CreateSecurityGroupInput{
			VpcId:     aws.String(c.vpcID),
			GroupName: aws.String(securityGroupName(c.clusterName)),
			TagSpecifications: []*ec2.TagSpecification{
				{
					Tags: []*ec2.Tag{
						{
							Key:   aws.String(key.AWSCniOperatorOwnedTag),
							Value: aws.String("owned"),
						},
						{
							Key:   aws.String("Name"),
							Value: aws.String(securityGroupName(c.clusterName)),
						},
					},
					ResourceType: aws.String("security-group"),
				},
			},
			Description: aws.String(fmt.Sprintf("aws cni security group for cluster %s", c.clusterName)),
		}

		o, err := ec2Client.CreateSecurityGroup(i)
		if err != nil {
			c.log.Error(err, "failed to create security group")
			return "", err
		}
		securityGroupID = *o.GroupId
		c.log.Info(fmt.Sprintf("created a new cni security group %s with id %s", securityGroupName(c.clusterName), securityGroupID))

	} else {
		c.log.Error(err, fmt.Sprintf("failed to fetch security group %s", securityGroupName(c.clusterName)))
		return "", err
	}

	i2 := &ec2.DescribeSecurityGroupRulesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: aws.StringSlice([]string{securityGroupRuleName(c.clusterName)}),
			},
		},
	}

	o2, err := ec2Client.DescribeSecurityGroupRules(i2)
	if err != nil {
		c.log.Error(err, fmt.Sprintf("failed to describe security group rules for security group %s", securityGroupName(c.clusterName)))
		return "", err
	}

	// create rules only if they are missing
	if len(o2.SecurityGroupRules) == 0 {
		var sgGroupPairs []*ec2.UserIdGroupPair
		// create security group ingress rule to allow traffic from each security group that is in the cluster
		for _, sg := range c.clusterSecurityGroupIDs {
			sgGroupPairs = append(sgGroupPairs, &ec2.UserIdGroupPair{GroupId: aws.String(sg)})
		}
		// also add aws-cni sg itself, so traffic can go across pods from different nodes
		sgGroupPairs = append(sgGroupPairs, &ec2.UserIdGroupPair{GroupId: aws.String(securityGroupID)})

		i := &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(securityGroupID),
			IpPermissions: []*ec2.IpPermission{
				{
					IpProtocol:       aws.String("-1"),
					FromPort:         aws.Int64(-1),
					ToPort:           aws.Int64(-1),
					UserIdGroupPairs: sgGroupPairs,
				},
			},
			TagSpecifications: []*ec2.TagSpecification{
				{
					Tags: []*ec2.Tag{
						{
							Key:   aws.String("Name"),
							Value: aws.String(securityGroupRuleName(c.clusterName)),
						},
					},
					ResourceType: aws.String("security-group-rule"),
				},
			},
		}
		_, err := ec2Client.AuthorizeSecurityGroupIngress(i)
		if IsAlreadyExists(err) {
			c.log.Info("security group ingress rule already exists")
		} else if err != nil {
			c.log.Error(err, "failed to create security group ingress rule")
			return "", err
		}
		c.log.Info(fmt.Sprintf("created a new security group ingress rule to allow traffic from %s security groups", c.clusterSecurityGroupIDs))
	} else {
		c.log.Info("security group ingress rules to allow traffic cni traffic already exists")
	}

	return securityGroupID, nil
}
*/
// applyENIConfigs will create or update ENIConfigs in the WC k8s api
func (c *CNIService) applyENIConfigs(subnets []CNISubnet, securityGroupID string) error {
	ctx := context.TODO()

	for _, s := range subnets {
		eniConfig := &v1alpha1.ENIConfig{
			TypeMeta: metav1.TypeMeta{
				APIVersion: v1alpha1.SchemeGroupVersion.String(),
				Kind:       "ENIConfig",
			},
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"giantswarm.io/docs": "https://godoc.org/github.com/aws/amazon-vpc-cni-k8s/pkg/apis/crd/v1alpha1#ENIConfig",
				},
				Name:      s.AZ,
				Namespace: corev1.NamespaceDefault,
			},
			Spec: v1alpha1.ENIConfigSpec{
				SecurityGroups: []string{
					securityGroupID,
				},
				Subnet: s.SubnetID,
			},
		}

		err := c.ctrlClient.Create(ctx, eniConfig)
		// check if wc k8s api is up yet
		if IsApiNotReadyYet(err) {
			c.log.Info("WC k8s api is not read yet")
			return errors.New("WC k8s api is not read yet")
		} else if k8serrors.IsAlreadyExists(err) {
			var latest v1alpha1.ENIConfig

			err := c.ctrlClient.Get(ctx, types.NamespacedName{Name: eniConfig.GetName(), Namespace: eniConfig.GetNamespace()}, &latest)
			if err != nil {
				c.log.Error(err, "failed to get eni configs")
				return err
			}

			eniConfig.ResourceVersion = latest.GetResourceVersion()

			err = c.ctrlClient.Update(ctx, eniConfig)
			if err != nil {
				c.log.Error(err, "failed to update eni config")
				return err
			}
		} else if err != nil {
			c.log.Error(err, "failed to create eni config")
			return err
		}
	}
	c.log.Info("applied ENIConfigs for aws cni")

	return nil
}

// Delete will clean any remaining CNI resources in WC VPC
func (c *CNIService) Delete() error {
	ec2Client := ec2.New(c.awsSession)

	err := c.deleteSecurityGroup(ec2Client)
	if err != nil {
		return err
	}

	err = c.deleteSubnets(ec2Client)
	if err != nil {
		return err
	}
	return nil
}

// deleteSecurityGroup deletes aws cni security group
func (c *CNIService) deleteSecurityGroup(ec2Client *ec2.EC2) error {
	// first we check if the security group  exist and fetch its ID
	inputDescribe := &ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:Name"),
				Values: aws.StringSlice([]string{securityGroupName(c.clusterName)}),
			},
			{
				Name:   aws.String(fmt.Sprintf("tag:%s", key.AWSCniOperatorOwnedTag)),
				Values: aws.StringSlice([]string{"owned"}),
			},
			{
				Name:   aws.String("vpc-id"),
				Values: aws.StringSlice([]string{c.vpcID}),
			},
		},
	}
	o, err := ec2Client.DescribeSecurityGroups(inputDescribe)
	if err != nil {
		c.log.Error(err, "failed to describe security group for deletion")
		return err
	}

	if len(o.SecurityGroups) > 0 {
		inputDelete := &ec2.DeleteSecurityGroupInput{
			GroupId: o.SecurityGroups[0].GroupId,
		}

		_, err = ec2Client.DeleteSecurityGroup(inputDelete)
		if IsNotFound(err) {
			//security group is already deleted, ignoring error
		} else if err != nil {
			c.log.Error(err, fmt.Sprintf("failed to delete security group %s", securityGroupName(c.clusterName)))
		}
	}

	return nil
}

// deleteSubnets will delete all CNI subnets from cluster VPC
func (c *CNIService) deleteSubnets(ec2Client *ec2.EC2) error {
	for _, az := range c.vpcAzList {
		describeInput := &ec2.DescribeSubnetsInput{
			Filters: []*ec2.Filter{
				{
					Name:   aws.String("tag:Name"),
					Values: aws.StringSlice([]string{subnetName(c.clusterName, az)}),
				},
				{
					Name:   aws.String(fmt.Sprintf("tag:%s", key.AWSCniOperatorOwnedTag)),
					Values: aws.StringSlice([]string{"owned"}),
				},
			},
		}
		o, err := ec2Client.DescribeSubnets(describeInput)
		if err != nil {
			c.log.Error(err, fmt.Sprintf("failed to describe subnet %s", subnetName(c.clusterName, az)))
			return err
		}

		if len(o.Subnets) != 0 {
			err := c.deleteSubnetNetworkInterfaces(ec2Client, *o.Subnets[0].SubnetId)
			if err != nil {
				return err
			}

			delInput := &ec2.DeleteSubnetInput{
				SubnetId: o.Subnets[0].SubnetId,
			}

			_, err = ec2Client.DeleteSubnet(delInput)
			if err != nil {
				c.log.Error(err, fmt.Sprintf("failed to delete subnet %s", subnetName(c.clusterName, az)))
				return err
			}
		}
	}
	return nil
}

// deleteSubnetNetworkInterfaces delete any remaining network interfaces from a subnet
func (c *CNIService) deleteSubnetNetworkInterfaces(ec2Client *ec2.EC2, subnetID string) error {
	i := &ec2.DescribeNetworkInterfacesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("subnet-id"),
				Values: aws.StringSlice([]string{subnetID}),
			},
		},
	}

	o, err := ec2Client.DescribeNetworkInterfaces(i)
	if err != nil {
		c.log.Error(err, "failed to describe network interfaces")
		return err
	}

	//detach ENIs
	for _, eni := range o.NetworkInterfaces {
		if eni.Attachment != nil {
			detachInput := &ec2.DetachNetworkInterfaceInput{
				Force:        aws.Bool(true),
				AttachmentId: eni.Attachment.AttachmentId,
			}
			// we ignore errors on detach in case the ENI was already detached or is detaching
			_, _ = ec2Client.DetachNetworkInterface(detachInput)
		}

	}

	//delete ENIs
	for _, eni := range o.NetworkInterfaces {
		delInput := &ec2.DeleteNetworkInterfaceInput{
			NetworkInterfaceId: eni.NetworkInterfaceId,
		}

		_, err := ec2Client.DeleteNetworkInterface(delInput)
		if err != nil {
			c.log.Error(err, fmt.Sprintf("failed to delete network interface %s", *eni.NetworkInterfaceId))
			return err
		}
	}

	return nil
}

func securityGroupName(clusterName string) string {
	return fmt.Sprintf("%s-aws-cni", clusterName)
}
func securityGroupRuleName(clusterName string) string {
	return fmt.Sprintf("%s-aws-cni-sg-rule", clusterName)
}
func subnetName(clusterName string, azName string) string {
	return fmt.Sprintf("%s-subnet-cni-%s", clusterName, azName)
}
