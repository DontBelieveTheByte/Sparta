// Copyright (c) 2015 Matt Weagle <mweagle@gmail.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// +build !lambdabinary

package sparta

import (
	"archive/zip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type workflowContext struct {
	serviceName          string
	serviceDescription   string
	lambdaAWSInfos       []*LambdaAWSInfo
	lambdaIAMRoleNameMap map[string]string
	s3Bucket             string
	s3LambdaZipKey       string
	logger               *logrus.Logger
}

type workflowStep func(ctx *workflowContext) (workflowStep, error)

/*
Return an AWS configuration option configured from the command line
http://docs.aws.amazon.com/cli/latest/userguide/cli-chap-getting-started.html
*/
func awsConfig() *aws.Config {
	region := os.Getenv("AWS_DEFAULT_REGION")
	if "" == region {
		region = "us-east-1"
	}
	return aws.NewConfig().WithRegion(region).WithMaxRetries(3)
}

// Verify & cache the IAM rolename to ARN mapping
func verifyIAMRoles(ctx *workflowContext) (workflowStep, error) {
	ctx.logger.Info("Verifying IAM Lambda execution roles")
	ctx.lambdaIAMRoleNameMap = make(map[string]string, 0)
	svc := iam.New(awsConfig())

	for _, eachLambda := range ctx.lambdaAWSInfos {
		_, exists := ctx.lambdaIAMRoleNameMap[eachLambda.ExecutionRoleName]
		if !exists {
			// Check the role
			params := &iam.GetRoleInput{
				RoleName: aws.String(eachLambda.ExecutionRoleName),
			}
			ctx.logger.Debug("Checking IAM RoleName: ", eachLambda.ExecutionRoleName)
			resp, err := svc.GetRole(params)
			if err != nil {
				ctx.logger.Error(err.Error())
				return nil, err
			}
			// Cache it - we'll need it later when we create the
			// CloudFormation template which needs the execution Arn (not role)
			ctx.lambdaIAMRoleNameMap[eachLambda.ExecutionRoleName] = *resp.Role.Arn
		}
	}
	ctx.logger.Info("IAM roles verified. Count: ", len(ctx.lambdaIAMRoleNameMap))
	return createPackageStep(), nil
}

// Return a string representation of a JS function call that can be exposed
// to AWS Lambda
func createNewNodeJSProxyEntry(lambdaInfo *LambdaAWSInfo, logger *logrus.Logger) string {
	// Create an entry of the form:
	// exports['foo'] = createForwarder('lambdaInfo.lambdaNama');
	nodeJSName := sanitizedName(lambdaInfo.lambdaFnName)
	logger.Info("Creating NodeJS function: " + nodeJSName)
	return fmt.Sprintf("exports[\"%s\"] = createForwarder(\"/%s\");\n",
		nodeJSName,
		lambdaInfo.lambdaFnName)
}

// Return the StackEvents for the given StackName/StackID
func stackEvents(stackID string, cfService *cloudformation.CloudFormation) ([]*cloudformation.StackEvent, error) {
	events := make([]*cloudformation.StackEvent, 0)
	nextToken := ""
	for {
		params := &cloudformation.DescribeStackEventsInput{
			StackName: aws.String(stackID),
		}
		if len(nextToken) > 0 {
			params.NextToken = aws.String(nextToken)
		}

		resp, err := cfService.DescribeStackEvents(params)
		if nil != err {
			return nil, err
		}
		events = append(events, resp.StackEvents...)
		if nil == resp.NextToken {
			break
		} else {
			nextToken = *resp.NextToken
		}
	}
	return events, nil
}

// Build and package the application
func createPackageStep() workflowStep {

	return func(ctx *workflowContext) (workflowStep, error) {
		// Compile the source to linux...
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		executableOutput := fmt.Sprintf("%s.lambda.amd64", sanitizedServiceName)
		cmd := exec.Command("go", "build", "-o", executableOutput, "-tags", "lambdabinary", ".")
		ctx.logger.Debug("Building application binary: ", cmd.Args)
		cmd.Env = os.Environ()
		cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64", "GO15VENDOREXPERIMENT=1")
		ctx.logger.Info("Compiling binary: ", executableOutput)

		outputWriter := ctx.logger.Writer()
		defer outputWriter.Close()
		cmd.Stdout = outputWriter
		cmd.Stderr = outputWriter

		err := cmd.Run()
		if err != nil {
			return nil, err
		}
		// Set the executable bit on the source
		defer os.Remove(executableOutput)

		// Binary size
		stat, err := os.Stat(executableOutput)
		if err != nil {
			return nil, errors.New("Failed to stat build output")
		}
		// Minimum hello world size is 2.3M
		// Minimum HTTP hello world is 6.3M
		ctx.logger.Debug("Executable binary size (MB): ", stat.Size()/(1024*1024))

		workingDir, err := os.Getwd()
		if err != nil {
			return nil, errors.New("Failed to retrieve working directory")
		}
		tmpFile, err := ioutil.TempFile(workingDir, sanitizedServiceName)
		if err != nil {
			return nil, errors.New("Failed to create temporary file")
		}

		defer func() {
			tmpFile.Close()
		}()

		ctx.logger.Info("Creating ZIP archive for upload: ", tmpFile.Name())
		lambdaArchive := zip.NewWriter(tmpFile)
		defer lambdaArchive.Close()

		// File info for the binary executable
		binaryWriter, err := lambdaArchive.Create(filepath.Base(executableOutput))
		if err != nil {
			return nil, errors.New("Failed to create ZIP entry: " + filepath.Base(executableOutput))
		}
		reader, err := os.Open(executableOutput)
		if err != nil {
			return nil, errors.New("Failed to open file: " + executableOutput)
		}
		defer reader.Close()
		io.Copy(binaryWriter, reader)

		// Add the string literal adapter, which requires us to add exported
		// functions to the end of index.js
		nodeJSWriter, err := lambdaArchive.Create("index.js")
		if err != nil {
			return nil, errors.New("Failed to create ZIP entry: index.js")
		}
		nodeJSSource := FSMustString(false, "/resources/index.js")
		nodeJSSource += "// DO NOT EDIT - CONTENT UNTIL EOF IS AUTOMATICALLY GENERATED\n"
		for _, eachLambda := range ctx.lambdaAWSInfos {
			nodeJSSource += createNewNodeJSProxyEntry(eachLambda, ctx.logger)
		}
		// Finally, replace
		// 	SPARTA_BINARY_NAME = 'Sparta.lambda.amd64';
		// with the service binary name
		nodeJSSource += fmt.Sprintf("SPARTA_BINARY_NAME='%s';\n", executableOutput)
		ctx.logger.Debug("Dynamically generated NodeJS adapter:\n", nodeJSSource)
		stringReader := strings.NewReader(nodeJSSource)
		io.Copy(nodeJSWriter, stringReader)
		// TODO: Zip template
		return createUploadStep(tmpFile.Name()), nil
	}
}

// Upload the
func createUploadStep(packagePath string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		ctx.logger.Info("Uploading ZIP archive to S3")

		reader, err := os.Open(packagePath)
		if err != nil {
			return nil, errors.New("Failed to upload to S3: " + err.Error())
		}
		defer func() {
			reader.Close()
			os.Remove(packagePath)
		}()

		s3Client := s3.New(awsConfig())
		uploadOptions := &s3manager.UploadOptions{S3: s3Client}

		body, err := os.Open(packagePath)
		if nil != err {
			return nil, err
		}
		keyName := filepath.Base(packagePath)
		uploadInput := &s3manager.UploadInput{
			Bucket:      &ctx.s3Bucket,
			Key:         &keyName,
			ContentType: aws.String("application/zip"),
			Body:        body,
		}
		uploader := s3manager.NewUploader(uploadOptions)
		result, err := uploader.Upload(uploadInput)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("ZIP archive uploaded: ", result.Location)
		// Cache it in case there was an error & we need to cleanup
		ctx.s3LambdaZipKey = keyName
		return ensureCloudFormationStack(keyName), nil
	}
}

// Does a given stack exist?
func stackExists(stackNameOrID string, logger *logrus.Logger) (bool, error) {
	awsCloudFormation := cloudformation.New(awsConfig())
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackNameOrID),
	}
	describeStacksOutput, err := awsCloudFormation.DescribeStacks(describeStacksInput)
	logger.Debug("DescribeStackOutput: ", describeStacksOutput)
	exists := false
	if err != nil {
		// If the stack doesn't exist, then no worries
		if strings.Contains(err.Error(), "does not exist") {
			exists = false
		} else {
			return false, err
		}
	} else {
		exists = true
	}
	return exists, nil
}

func convergeStackState(cfTemplateURL string, ctx *workflowContext) (*cloudformation.Stack, error) {

	// Does it exist?
	exists, err := stackExists(ctx.serviceName, ctx.logger)
	if nil != err {
		return nil, err
	}
	awsCloudFormation := cloudformation.New(awsConfig())
	stackID := ""
	if exists {
		// Update stack
		updateStackInput := &cloudformation.UpdateStackInput{
			StackName:   aws.String(ctx.serviceName),
			TemplateURL: aws.String(cfTemplateURL),
		}
		updateStackResponse, err := awsCloudFormation.UpdateStack(updateStackInput)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("Issued update request: ", *updateStackResponse.StackId)
		stackID = *updateStackResponse.StackId
	} else {
		// Create stack
		createStackInput := &cloudformation.CreateStackInput{
			StackName:        aws.String(ctx.serviceName),
			TemplateURL:      aws.String(cfTemplateURL),
			TimeoutInMinutes: aws.Int64(5),
			OnFailure:        aws.String(cloudformation.OnFailureDelete),
		}
		createStackResponse, err := awsCloudFormation.CreateStack(createStackInput)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("Creating stack: ", *createStackResponse.StackId)
		stackID = *createStackResponse.StackId
	}

	// Poll for the current stackID state
	describeStacksInput := &cloudformation.DescribeStacksInput{
		StackName: aws.String(stackID),
	}

	var stackInfo *cloudformation.Stack
	stackOperationComplete := false
	ctx.logger.Info("Waiting for stack to complete")
	for !stackOperationComplete {
		time.Sleep(10 * time.Second)
		describeStacksOutput, err := awsCloudFormation.DescribeStacks(describeStacksInput)
		if nil != err {
			return nil, err
		}
		if len(describeStacksOutput.Stacks) > 0 {
			stackInfo = describeStacksOutput.Stacks[0]
			ctx.logger.Info("Current state: ", *stackInfo.StackStatus)
			switch *stackInfo.StackStatus {
			case cloudformation.StackStatusCreateInProgress,
				cloudformation.StackStatusDeleteInProgress,
				cloudformation.StackStatusUpdateInProgress,
				cloudformation.StackStatusRollbackInProgress,
				cloudformation.StackStatusUpdateCompleteCleanupInProgress,
				cloudformation.StackStatusUpdateRollbackCompleteCleanupInProgress,
				cloudformation.StackStatusUpdateRollbackInProgress:
				time.Sleep(20 * time.Second)
			default:
				stackOperationComplete = true
				break
			}
		} else {
			return nil, errors.New("More than one stack returned for: " + stackID)
		}
	}
	// What happened?
	succeed := true
	switch *stackInfo.StackStatus {
	case cloudformation.StackStatusDeleteComplete, // Initial create failure
		cloudformation.StackStatusUpdateRollbackComplete: // Update failure
		succeed = false
	default:
		succeed = true
	}

	// If it didn't work, then output some failure information
	if !succeed {
		// Get the stack events and find the ones that failed.
		events, err := stackEvents(stackID, awsCloudFormation)
		if nil != err {
			return nil, err
		}
		ctx.logger.Error("Stack provisioning failed.")
		for _, eachEvent := range events {
			switch *eachEvent.ResourceStatus {
			case cloudformation.ResourceStatusCreateFailed,
				cloudformation.ResourceStatusDeleteFailed,
				cloudformation.ResourceStatusUpdateFailed:
				errMsg := fmt.Sprintf("\tError ensuring %s (%s): %s",
					*eachEvent.ResourceType,
					*eachEvent.LogicalResourceId,
					*eachEvent.ResourceStatusReason)
				ctx.logger.Error(errMsg)
			default:
				// NOP
			}
		}
		return nil, errors.New("Failed to provision: " + ctx.serviceName)
	} else {
		return stackInfo, nil
	}
}

func ensureCloudFormationStack(s3Key string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		awsConfig := awsConfig()

		// We're going to create a template that represents the new state of the
		// lambda world.
		cloudFormationTemplate := ArbitraryJSONObject{
			"AWSTemplateFormatVersion": "2010-09-09",
			"Description":              ctx.serviceDescription,
		}
		resources := make(ArbitraryJSONObject, 0)
		for _, eachEntry := range ctx.lambdaAWSInfos {
			err := eachEntry.toCloudFormationResources(ctx.s3Bucket, s3Key, ctx.lambdaIAMRoleNameMap, resources)
			if nil != err {
				return nil, err
			}
		}
		cloudFormationTemplate["Resources"] = resources

		// Generate a complete CloudFormation template
		cfTemplate, err := json.Marshal(cloudFormationTemplate)
		if err != nil {
			ctx.logger.Error("Failed to Marshal CloudFormation template: ", err.Error())
			return nil, err
		}

		// Upload the template to S3
		s3Client := s3.New(awsConfig)
		uploadOptions := &s3manager.UploadOptions{S3: s3Client}
		contentBody := string(cfTemplate)
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		hash := sha1.New()
		hash.Write([]byte(contentBody))
		s3keyName := fmt.Sprintf("%s-%s-cf.json", sanitizedServiceName, hex.EncodeToString(hash.Sum(nil)))

		ctx.logger.Info("Uploading CloudFormation template")

		uploadInput := &s3manager.UploadInput{
			Bucket:      &ctx.s3Bucket,
			Key:         &s3keyName,
			ContentType: aws.String("application/json"),
			Body:        strings.NewReader(contentBody),
		}
		ctx.logger.Debug("Cloudformation template:\n", contentBody)
		uploader := s3manager.NewUploader(uploadOptions)
		templateUploadResult, err := uploader.Upload(uploadInput)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("CloudFormation template uploaded: ", templateUploadResult.Location)
		stack, err := convergeStackState(templateUploadResult.Location, ctx)
		if nil != err {
			return nil, err
		}
		ctx.logger.Info("Stack provisioned: ", stack)
		return nil, nil
	}
}

// Compiles, packages, and provisions (either create or update) a Sparta application. The serviceName is the service's logical
// name and is used to distinguish between create and update operations.  The compilation options/flags are:
//
// 	TAGS:         -tags lambdabinary
// 	ENVIRONMENT:  GOOS=linux GOARCH=amd64 GO15VENDOREXPERIMENT=1
//
// The compiled binary is packaged with a NodeJS proxy shim to manage AWS Lambda setup & invocation per
// http://docs.aws.amazon.com/lambda/latest/dg/authoring-function-in-nodejs.html
//
// The two files are ZIP'd, posted to S3 and used as an input to a dynamically generated CloudFormation
// template (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/Welcome.html)
// which creates or updates the service state.
//
// More information on golang 1.5's support for vendor'd resources is documented at
//
//  https://docs.google.com/document/d/1Bz5-UB7g2uPBdOx-rw5t9MxJwkfpx90cqG9AFL0JAYo/edit
//  https://medium.com/@freeformz/go-1-5-s-vendor-experiment-fd3e830f52c3#.voiicue1j
//
func Provision(serviceName string, serviceDescription string, lambdaAWSInfos []*LambdaAWSInfo, s3Bucket string, logger *logrus.Logger) error {
	ctx := &workflowContext{
		serviceName:        serviceName,
		serviceDescription: serviceDescription,
		lambdaAWSInfos:     lambdaAWSInfos,
		s3Bucket:           s3Bucket,
		logger:             logger}

	for step := verifyIAMRoles; step != nil; {
		next, err := step(ctx)
		if err != nil {
			ctx.logger.Error(err.Error())
			if "" != ctx.s3LambdaZipKey {
				ctx.logger.Info("Attempting to cleanup ZIP archive: ", ctx.s3LambdaZipKey)
				s3Client := s3.New(awsConfig())
				params := &s3.DeleteObjectInput{
					Bucket: aws.String(ctx.s3Bucket),
					Key:    aws.String(ctx.s3LambdaZipKey),
				}
				_, err := s3Client.DeleteObject(params)
				if nil != err {
					ctx.logger.Warn("Failed to delete archive")
				}
			}
			return err
		}
		if next == nil {
			break
		} else {
			step = next
		}
	}
	return nil
}