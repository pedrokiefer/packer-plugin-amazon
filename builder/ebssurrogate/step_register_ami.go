package ebssurrogate

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awscommon "github.com/hashicorp/packer-plugin-amazon/builder/common"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/random"
	confighelper "github.com/hashicorp/packer-plugin-sdk/template/config"
)

// StepRegisterAMI creates the AMI.
type StepRegisterAMI struct {
	PollingConfig            *awscommon.AWSPollingConfig
	RootDevice               RootBlockDevice
	AMIDevices               []*ec2.BlockDeviceMapping
	LaunchDevices            []*ec2.BlockDeviceMapping
	EnableAMIENASupport      confighelper.Trilean
	EnableAMISriovNetSupport bool
	Architecture             string
	image                    *ec2.Image
	LaunchOmitMap            map[string]bool
	AMISkipBuildRegion       bool
	BootMode                 string
}

func (s *StepRegisterAMI) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	ec2conn := state.Get("ec2").(*ec2.EC2)
	snapshotIds := state.Get("snapshot_ids").(map[string]string)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Registering the AMI...")

	blockDevices := s.combineDevices(snapshotIds)

	// Create the image
	amiName := config.AMIName
	state.Put("intermediary_image", false)
	if config.AMIEncryptBootVolume.True() || s.AMISkipBuildRegion {
		state.Put("intermediary_image", true)

		// From AWS SDK docs: You can encrypt a copy of an unencrypted snapshot,
		// but you cannot use it to create an unencrypted copy of an encrypted
		// snapshot. Your default CMK for EBS is used unless you specify a
		// non-default key using KmsKeyId.

		// If encrypt_boot is nil or true, we need to create a temporary image
		// so that in step_region_copy, we can copy it with the correct
		// encryption
		amiName = random.AlphaNum(7)
	}

	registerOpts := &ec2.RegisterImageInput{
		Name:                &amiName,
		Architecture:        aws.String(s.Architecture),
		RootDeviceName:      aws.String(s.RootDevice.DeviceName),
		VirtualizationType:  aws.String(config.AMIVirtType),
		BlockDeviceMappings: blockDevices,
	}

	if s.EnableAMISriovNetSupport {
		// Set SriovNetSupport to "simple". See http://goo.gl/icuXh5
		// As of February 2017, this applies to C3, C4, D2, I2, R3, and M4 (excluding m4.16xlarge)
		registerOpts.SriovNetSupport = aws.String("simple")
	}
	if s.EnableAMIENASupport.True() {
		// Set EnaSupport to true
		// As of February 2017, this applies to C5, I3, P2, R4, X1, and m4.16xlarge
		registerOpts.EnaSupport = aws.Bool(true)
	}
	if s.BootMode != "" {
		registerOpts.BootMode = aws.String(s.BootMode)
	}
	registerResp, err := ec2conn.RegisterImage(registerOpts)
	if err != nil {
		state.Put("error", fmt.Errorf("Error registering AMI: %s", err))
		ui.Error(state.Get("error").(error).Error())
		return multistep.ActionHalt
	}

	// Set the AMI ID in the state
	ui.Say(fmt.Sprintf("AMI: %s", *registerResp.ImageId))
	amis := make(map[string]string)
	amis[*ec2conn.Config.Region] = *registerResp.ImageId
	state.Put("amis", amis)

	// Wait for the image to become ready
	ui.Say("Waiting for AMI to become ready...")
	if err := s.PollingConfig.WaitUntilAMIAvailable(ctx, ec2conn, *registerResp.ImageId); err != nil {
		err := fmt.Errorf("Error waiting for AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	imagesResp, err := ec2conn.DescribeImages(&ec2.DescribeImagesInput{ImageIds: []*string{registerResp.ImageId}})
	if err != nil {
		err := fmt.Errorf("Error searching for AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	s.image = imagesResp.Images[0]

	snapshots := make(map[string][]string)
	for _, blockDeviceMapping := range imagesResp.Images[0].BlockDeviceMappings {
		if blockDeviceMapping.Ebs != nil && blockDeviceMapping.Ebs.SnapshotId != nil {

			snapshots[*ec2conn.Config.Region] = append(snapshots[*ec2conn.Config.Region], *blockDeviceMapping.Ebs.SnapshotId)
		}
	}
	state.Put("snapshots", snapshots)

	return multistep.ActionContinue
}

func (s *StepRegisterAMI) Cleanup(state multistep.StateBag) {
	if s.image == nil {
		return
	}

	_, cancelled := state.GetOk(multistep.StateCancelled)
	_, halted := state.GetOk(multistep.StateHalted)
	if !cancelled && !halted {
		return
	}

	ec2conn := state.Get("ec2").(*ec2.EC2)
	ui := state.Get("ui").(packersdk.Ui)

	ui.Say("Deregistering the AMI because cancellation or error...")
	deregisterOpts := &ec2.DeregisterImageInput{ImageId: s.image.ImageId}
	if _, err := ec2conn.DeregisterImage(deregisterOpts); err != nil {
		ui.Error(fmt.Sprintf("Error deregistering AMI, may still be around: %s", err))
		return
	}
}

func (s *StepRegisterAMI) combineDevices(snapshotIds map[string]string) []*ec2.BlockDeviceMapping {
	devices := map[string]*ec2.BlockDeviceMapping{}

	for _, device := range s.AMIDevices {
		devices[*device.DeviceName] = device
	}

	// Devices in launch_block_device_mappings override any with
	// the same name in ami_block_device_mappings, except for the
	// one designated as the root device in ami_root_device
	for _, device := range s.LaunchDevices {
		// Skip devices we've flagged for omission
		omit, ok := s.LaunchOmitMap[*device.DeviceName]
		if ok && omit {
			continue
		}
		snapshotId, ok := snapshotIds[*device.DeviceName]
		if ok {
			device.Ebs.SnapshotId = aws.String(snapshotId)
			// Block devices with snapshot inherit
			// encryption settings from the snapshot
			device.Ebs.Encrypted = nil
			device.Ebs.KmsKeyId = nil
		}
		if *device.DeviceName == s.RootDevice.SourceDeviceName {
			device.DeviceName = aws.String(s.RootDevice.DeviceName)
		}
		devices[*device.DeviceName] = device
	}

	blockDevices := []*ec2.BlockDeviceMapping{}
	for _, device := range devices {
		blockDevices = append(blockDevices, device)
	}
	return blockDevices
}
