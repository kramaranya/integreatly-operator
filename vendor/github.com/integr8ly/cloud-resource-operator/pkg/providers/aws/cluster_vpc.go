// Cluster VPC and networking setup
//
// This file contains functions used to setup additional private sub-networks in an OpenShift cluster VPC
// The reason this file is required is due to CRO provisioning multi-AZ resources for AWS, even though the cluster it's
// provisioning resources for could be single AZ
//
// Problem statement:
//   - A single-AZ OpenShift cluster exists in a VPC, in a single AZ, which contains a public and a private sub-network
//   - A multi-AZ RDS instance for that OpenShift cluster exists in the same VPC, in at least 2 AZ's which must contain
//     at least a private sub-network
//   - By default, if we try to provision a multi-AZ RDS instance in the cluster VPC it will fail as there is only one
//     private sub-network available in one AZ
//
// To allow for this, we must create the missing private sub-networks in the AZ's that the cluster is not provisioned
// in, so that RDS can successfully provision
//
// This file provides functions that help check if a private subnet already exists in an AZ, if it does not, it can
// create one. If an OpenShift cluster is already multi-AZ, there will be enough sub-networks for RDS and no additional
// private sub-networks should be created
package aws

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/integr8ly/cloud-resource-operator/pkg/resources"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	errorUtil "github.com/pkg/errors"
)

const (
	defaultAWSPrivateSubnetTagKey = "kubernetes.io/role/internal-elb"
	defaultSecurityGroupPostfix   = "security-group"
	defaultSubnetGroupDesc        = "Subnet group created and managed by the Cloud Resource Operator"
	// In AWS this must be between 16 and 28
	// We want to use the least host addresses possible, so that we can support clusters provisioned in VPCs with small CIDR masks
	// 28 has too few hosts available to be future-proof for RHOAM products, so use 27 to avoid a migration being required in the future
	// Note: The larger the mask, the less hosts available in the network
	defaultSubnetMask    = 27
	defaultSubnetPostfix = "subnet-group"
)

// ensures a subnet group is in place for the creation of a resource
func configureSecurityGroup(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API, logger *logrus.Entry) error {
	// get cluster id
	clusterID, err := resources.GetClusterID(ctx, c)
	if err != nil {
		return errorUtil.Wrap(err, "error getting cluster id")
	}
	logger.Infof("ensuring security group is correct for cluster %s", clusterID)

	// build security group name
	secName, err := resources.BuildInfraName(ctx, c, defaultSecurityGroupPostfix, defaultAwsIdentifierLength)
	logger.Info(fmt.Sprintf("setting resource security group %s", secName))
	if err != nil {
		return errorUtil.Wrap(err, "error building subnet group name")
	}

	// get cluster cidr group
	vpcID, cidr, err := GetCidr(ctx, c, ec2Svc, logger)
	if err != nil {
		return errorUtil.Wrap(err, "error finding cidr block")
	}

	foundSecGroup, err := getSecurityGroup(ec2Svc, secName)
	if err != nil {
		return errorUtil.Wrap(err, "error get security group")
	}

	if foundSecGroup == nil {
		// create security group
		logger.Infof("creating security group from cluster %s", clusterID)
		if _, err := ec2Svc.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
			Description: aws.String(fmt.Sprintf("security group for cluster %s", clusterID)),
			GroupName:   aws.String(secName),
			VpcId:       aws.String(vpcID),
		}); err != nil {
			return errorUtil.Wrap(err, "error creating security group")
		}
		return nil
	}
	logger.Infof("found security group %s for cluster %s", *foundSecGroup.GroupId, clusterID)

	// build ip permission
	ipPermission := &ec2.IpPermission{
		IpProtocol: aws.String("-1"),
		IpRanges: []*ec2.IpRange{
			{
				CidrIp: aws.String(cidr),
			},
		},
	}

	// check if correct permissions are in place
	for _, perm := range foundSecGroup.IpPermissions {
		if reflect.DeepEqual(perm, ipPermission) {
			logger.Infof("ip permissions are correct for security group %s", *foundSecGroup.GroupName)
			return nil
		}
	}

	// authorize ingress
	logger.Infof("setting ingress ip permissions for %s", *foundSecGroup.GroupName)
	if _, err := ec2Svc.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
		GroupId: aws.String(*foundSecGroup.GroupId),
		IpPermissions: []*ec2.IpPermission{
			ipPermission,
		},
	}); err != nil {
		return errorUtil.Wrap(err, "error authorizing security group ingress")
	}

	return nil
}

// GetVPCSubnets returns a list of subnets associated with cluster VPC
func GetVPCSubnets(ec2Svc ec2iface.EC2API, logger *logrus.Entry, vpc *ec2.Vpc) ([]*ec2.Subnet, error) {
	logger.Info("gathering cluster vpc and subnet information")
	// poll subnets to ensure credentials have reconciled
	subs, err := getSubnets(ec2Svc)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting subnets")
	}

	if vpc == nil {
		return nil, errorUtil.Wrap(err, "vpc is nil, need vpc to find associated subnets")
	}
	// find associated subnets
	var associatedSubs []*ec2.Subnet
	for _, sub := range subs {
		if *sub.VpcId == *vpc.VpcId {
			associatedSubs = append(associatedSubs, sub)
		}
	}

	// check if found subnets associated with cluster vpc
	if associatedSubs == nil {
		return nil, errorUtil.New("error, unable to find subnets associated with cluster vpc")
	}

	return associatedSubs, nil
}

// GetSubnetIDS returns a list of subnet ids associated with cluster vpc
func GetPrivateSubnetIDS(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API, logger *logrus.Entry) ([]*string, error) {
	logger.Info("gathering all private subnets in cluster vpc")
	// get cluster vpc
	foundVPC, err := getClusterVpc(ctx, c, ec2Svc, logger)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting vpcs")
	}

	// get subnets in vpc
	subs, err := GetVPCSubnets(ec2Svc, logger, foundVPC)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting vpc subnets")
	}

	// get a list of availability zones
	azs, err := getAZs(ec2Svc)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting availability zones")
	}

	// filter based on a tag key attached to private subnets
	var privSubs []*ec2.Subnet
	for _, sub := range subs {
		for _, tags := range sub.Tags {
			if *tags.Key == defaultAWSPrivateSubnetTagKey {
				privSubs = append(privSubs, sub)
			}
		}
	}

	// for every az check there is a private subnet, if none create one
	for _, az := range azs {
		logger.Infof("checking if private subnet exists in zone %s", *az.ZoneName)
		if !privateSubnetExists(privSubs, az) {
			logger.Infof("no private subnet found in %s", *az.ZoneName)
			subnet, err := createPrivateSubnet(ctx, c, ec2Svc, foundVPC, logger, *az.ZoneName)
			if err != nil {
				return nil, errorUtil.Wrap(err, "failed to created private subnet")
			}
			privSubs = append(privSubs, subnet)
		}
	}

	// build list of subnet ids
	var subIDs []*string
	for _, sub := range privSubs {
		subIDs = append(subIDs, sub.SubnetId)
	}

	if subIDs == nil {
		return nil, errorUtil.New("failed to get list of private subnet ids")
	}

	return subIDs, nil
}

// checks is a private subnet exists and is available in an availability zone
func privateSubnetExists(privSubs []*ec2.Subnet, zone *ec2.AvailabilityZone) bool {
	for _, subnet := range privSubs {
		if *subnet.AvailabilityZone == *zone.ZoneName && *zone.State == "available" {
			return true
		}
	}
	return false
}

// creates and tags a private subnet
func createPrivateSubnet(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API, vpc *ec2.Vpc, logger *logrus.Entry, zone string) (*ec2.Subnet, error) {
	// get list of potential subnet addresses
	logger.Infof("creating private subnet in %s", *vpc.VpcId)
	subs, err := buildSubnetAddress(vpc, logger)
	if err != nil {
		return nil, errorUtil.Wrap(err, "failed to build subnets")
	}

	// create subnet looping through potential subnet list
	var subnet *ec2.Subnet
	for _, ip := range subs {
		logger.Infof("attempting to create subnet with cidr block %s for vpc %s in zone %s", ip.String(), *vpc.VpcId, zone)
		createOutput, err := ec2Svc.CreateSubnet(&ec2.CreateSubnetInput{
			AvailabilityZone: aws.String(zone),
			CidrBlock:        aws.String(ip.String()),
			VpcId:            aws.String(*vpc.VpcId),
		})
		ec2err, isAwsErr := err.(awserr.Error)
		if err != nil && isAwsErr && ec2err.Code() == "InvalidSubnet.Conflict" {
			logger.Infof("%s conflicts with a current subnet, trying again", ip)
			continue
		}
		if err != nil {
			return nil, errorUtil.Wrap(err, "error creating new subnet")
		}
		if newErr := tagPrivateSubnet(ctx, c, ec2Svc, createOutput.Subnet, logger); newErr != nil {
			return nil, newErr
		}
		logger.Infof("created new subnet %s in %s", ip, *vpc.VpcId)
		subnet = createOutput.Subnet
		break
	}
	if subnet == nil {

	}
	return subnet, nil
}

// tags a private subnet with the default aws private subnet tag
func tagPrivateSubnet(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API, sub *ec2.Subnet, logger *logrus.Entry) error {
	logger.Infof("tagging cloud resource subnet %s", *sub.SubnetId)
	tags, err := getDefaultSubnetTags(ctx, c)
	if err != nil {
		return err
	}
	_, err = ec2Svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{
			aws.String(*sub.SubnetId),
		},
		Tags: tags,
	})
	if err != nil {
		return errorUtil.Wrap(err, "failed to tag subnet")
	}
	return nil
}

// retrieves default subnet tags
func getDefaultSubnetTags(ctx context.Context, c client.Client) ([]*ec2.Tag, error) {
	// get cluster id
	clusterID, err := resources.GetClusterID(ctx, c)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting clusterID")
	}
	organizationTag := resources.GetOrganizationTag()

	tags := []*resources.Tag{
		{
			Key:   defaultAWSPrivateSubnetTagKey,
			Value: "1",
		}, {
			Key:   fmt.Sprintf("%sclusterID", organizationTag),
			Value: clusterID,
		}, {
			Key:   resources.TagDisplayName,
			Value: defaultSubnetNameTagValue,
		}, resources.BuildManagedTag(),
	}
	infraTags, err := resources.GetUserInfraTags(ctx, c)
	if err != nil {
		msg := "Failed to get user infrastructure tags"
		return nil, errorUtil.Wrapf(err, msg)
	}
	if infraTags != nil {
		// merge tags into single array, where any duplicate
		// values in infra are discarded in favour of the default tags
		tags = resources.MergeTags(tags, infraTags)
	}

	return genericListToEc2TagList(tags), nil
}

// Builds a list of valid subnet CIDR blocks
// Valid meaning it:
// - Exists within the cluster VPC CIDR block
// - Supports the amount of hosts that CRO requires by default for all RHOAM products
func buildSubnetAddress(vpc *ec2.Vpc, logger *logrus.Entry) ([]net.IPNet, error) {
	logger.Infof("calculating subnet mask and address for vpc cidr %s", *vpc.CidrBlock)
	if *vpc.CidrBlock == "" {
		return nil, errorUtil.New("vpc cidr block can't be empty")
	}

	// AWS stores it's CIDR block as a string, convert it
	_, awsCIDR, err := net.ParseCIDR(*vpc.CidrBlock)
	if err != nil {
		return nil, errorUtil.Wrapf(err, "failed to parse vpc cidr block %s", *vpc.CidrBlock)
	}
	// Get the cluster VPC mask size
	// e.g. If the cluster VPC CIDR block is 10.0.0.0/8, the size is 8 (8 bits)
	maskSize, _ := awsCIDR.Mask.Size()

	// If the VPC CIDR mask size is greater or equal to the size that CRO requires
	// - If equal, CRO will not be able to subdivide the VPC CIDR into sub-networks
	// - If greater, there will be fewer host addresses available in the sub-networks than CRO needs
	// Note: The larger the mask size, the less hosts the network can support
	if maskSize >= defaultSubnetMask {
		return nil, errorUtil.New(fmt.Sprintf("vpc cidr block %s cannot contain generated subnet mask /%d", *vpc.CidrBlock, defaultSubnetMask))
	}

	// Create the smallest possible CIDR block that CRO can use
	croCIDRStr := fmt.Sprintf("%s/%d", awsCIDR.IP.String(), defaultSubnetMask)
	_, croCIDR, err := net.ParseCIDR(croCIDRStr)
	if err != nil {
		return nil, errorUtil.Wrapf(err, "failed to parse cro cidr block %s", croCIDRStr)
	}

	// Generate all possible valid sub-networks that can be used in the cluster VPC CIDR range
	networks := generateAvailableSubnets(awsCIDR, croCIDR)

	// Reverse the network list as the end networks are more likely to be unused, small optimisation
	for i, j := 0, len(networks)-1; i < j; i, j = i+1, j-1 {
		networks[i], networks[j] = networks[j], networks[i]
	}
	return networks, nil
}

func generateAvailableSubnets(fromCIDR, toCIDR *net.IPNet) []net.IPNet {
	toIPv4 := toCIDR.IP.To4()
	networks := []net.IPNet{
		{
			IP:   toIPv4,
			Mask: toCIDR.Mask,
		},
	}
	// The #Contains check here is done to ensure we don't bother generating subnet addresses outside of the scope of
	// the fromCIDR
	// e.g. If fromCIDR is 10.0.0.0/8, there's no reason to try to generate any ranges outside of 10.x.x.x as they
	// won't be valid sub-network addresses of fromCIDR
	for i := 0; fromCIDR.Contains(incrementIP(toIPv4, i)); i++ {
		// Get the next IP address
		nextFoundNetwork := incrementIP(toIPv4, i)
		// Ensure the network is a valid sub-network in toCIDR
		// We only want valid sub-network addresses between toCIDR and fromCIDR, we don't want to store host addresses
		// We don't need duplicates
		nextFoundNetworkMasked := nextFoundNetwork.Mask(toCIDR.Mask)
		if containsNetwork(networks, nextFoundNetworkMasked) {
			continue
		}

		// Ensure the network is added with the mask of toCIDR
		// e.g.
		// Cluster VPC (fromCIDR) is 10.0.0.0/8
		// toCIDR is 10.0.0.0/24
		// we want all possible /24 networks that are valid between fromCIDR and toCIDR
		networks = append(networks, net.IPNet{
			IP:   nextFoundNetworkMasked,
			Mask: toCIDR.Mask,
		})
	}
	return networks
}

func containsNetwork(networks []net.IPNet, toFind net.IP) bool {
	for _, n := range networks {
		if n.IP.Equal(toFind) {
			return true
		}
	}
	return false
}

// Increment an IP address by a provided increment value
// Makes cycling through IP addresses simple as we can keep incrememnting by 1 to iterate through a range of IPs
func incrementIP(ip net.IP, inc int) net.IP {
	// It's not guaranteed that a provided IP will be in IPv4 format, we need to be able to split it into bytes. So
	// ensure it is formatted correctly first.
	ipv4 := ip.To4()
	// Join the four separate bytes of the IP address into one int, this makes cycling through IP addresses easy as
	// when we overflow on one byte, the byte will be reset to 00000000 for us and the next byte will be incremented
	//
	// The << operator is shifting the byte up the int, so the bits don't override one another
	// e.g. uint8(byte(1)) << 4, would be 16, because the original byte (00000001) would be shifted by 4 bits (00010000)
	//
	// Use an unsigned integer so that we don't have a wasted bit on the integer
	joinedBytes := uint(ipv4[0])<<24 + uint(ipv4[1])<<16 + uint(ipv4[2])<<8 + uint(ipv4[3])
	// Add inc to the joined bytes
	joinedBytes += uint(inc)
	// Unshift the joined bytes integer back into the original 4 bytes of an IP address
	//
	// The >> operator is the reverse shifting direction to <<
	// e.g. uint(byte(16)) >> 4 would be 1, because the original byte (00010000) would be shifted by 4 bits (00000001)
	//
	// The & operator is a bitwise AND operation, it's used to mask (keep) the last byte in the uint and ignore the
	// rest. 0xFF is all 0's except the last 2 nibbles (8 bits, 1 byte)
	// e.g. byte(17) & OxF would be 1, because the original byte (00010001) would be AND'd with (00001111), leaving
	// (00000001)
	//
	// The final byte conversion just ensures we're only taking the last 8 bits of the shifted and AND'd uint
	byte3 := byte(joinedBytes & 0xFF)
	byte2 := byte((joinedBytes >> 8) & 0xFF)
	byte1 := byte((joinedBytes >> 16) & 0xFF)
	byte0 := byte((joinedBytes >> 24) & 0xFF)
	// Convert the 4 bytes into an IP address
	return net.IPv4(byte0, byte1, byte2, byte3)
}

// returns vpc id and cidr block for found vpc
func GetCidr(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API, logger *logrus.Entry) (string, string, error) {
	foundVPC, err := getClusterVpc(ctx, c, ec2Svc, logger)
	if err != nil {
		return "", "", errorUtil.Wrap(err, "error getting vpcs")
	}

	// check if found cluster vpc
	if foundVPC == nil {
		return "", "", errorUtil.New("error, unable to find a vpc")
	}

	return *foundVPC.VpcId, *foundVPC.CidrBlock, nil
}

// function to get AZ
func getAZs(ec2Svc ec2iface.EC2API) ([]*ec2.AvailabilityZone, error) {
	azs, err := ec2Svc.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{})
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting availability zones")
	}
	return azs.AvailabilityZones, nil
}

// function to get subnets, used to check/wait on AWS credentials
func getSubnets(ec2Svc ec2iface.EC2API) ([]*ec2.Subnet, error) {
	var subs []*ec2.Subnet
	err := wait.PollImmediate(time.Second*5, time.Minute*5, func() (done bool, err error) {
		listOutput, err := ec2Svc.DescribeSubnets(&ec2.DescribeSubnetsInput{})
		if err != nil {
			return false, nil
		}
		subs = listOutput.Subnets
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return subs, nil
}

func getVPCIDByClusterSubnets(ec2Svc ec2iface.EC2API, clusterID string) (string, error) {
	listOutput, err := ec2Svc.DescribeSubnets(&ec2.DescribeSubnetsInput{})
	if err != nil {
		return "", err
	}

	for _, sub := range listOutput.Subnets {
		for _, tag := range sub.Tags {
			if tag != nil && (*tag.Value == "owned" || *tag.Value == "shared") &&
				*tag.Key == getOSDClusterTagKey(clusterID) {
				return *sub.VpcId, nil
			}
		}
	}
	return "", errorUtil.New(fmt.Sprintf("failed to get cluster vpc id, no vpc found with osd cluster tag: could not find cluster associated subnets with clusterID %s", clusterID))
}

// function to get vpc of a cluster
func getClusterVpc(ctx context.Context, c client.Client, ec2Svc ec2iface.EC2API, logger *logrus.Entry) (*ec2.Vpc, error) {
	// first call to aws api from the network provider is to get cluster vpc
	// polling to allow credential minter time to reconcile credentials

	// get cluster id
	clusterID, err := resources.GetClusterID(ctx, c)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting clusterID")
	}

	vpcId, err := getVPCIDByClusterSubnets(ec2Svc, clusterID)
	if err != nil {
		return nil, errorUtil.Wrap(err, "error getting vpc id from associated subnets")
	}

	vpcs, err := ec2Svc.DescribeVpcs(&ec2.DescribeVpcsInput{VpcIds: []*string{aws.String(vpcId)}})
	if err != nil {
		return nil, errorUtil.Wrap(err, fmt.Sprintf("error getting vpc with id %s", vpcId))
	}

	if len(vpcs.Vpcs) == 0 {
		return nil, errorUtil.New("error, no vpc found")
	}
	if len(vpcs.Vpcs) > 1 {
		return nil, errorUtil.New("error, more than one vpc found associated with cluster subnets")
	}
	logger.Infof("found cluster %s vpc %s", clusterID, *vpcs.Vpcs[0].VpcId)
	return vpcs.Vpcs[0], nil
}

// getSecurityGroup a utility function for returning cro resource security group
// we filter security groups based on a pre-determined security group name
// if a security group does not exist a nil object is returned
func getSecurityGroup(ec2Svc ec2iface.EC2API, secName string) (*ec2.SecurityGroup, error) {
	// get security groups
	secGroups, err := ec2Svc.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{})
	if err != nil {
		return nil, errorUtil.Wrap(err, "failed to return information about security groups")
	}

	// check if security group exists
	var foundSecGroup *ec2.SecurityGroup
	for _, sec := range secGroups.SecurityGroups {
		if *sec.GroupName == secName {
			foundSecGroup = sec
			break
		}
	}

	return foundSecGroup, nil
}

func getOSDClusterTagKey(clusterID string) string {
	return fmt.Sprintf("%s%s", clusterOwnedTagKeyPrefix, clusterID)
}
