/*
Copyright 2016 The Kubernetes Authors.

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

package remote

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	tracing "k8s.io/component-base/tracing"
	"k8s.io/klog/v2"

	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/util"
)

// remoteImageService is a gRPC implementation of internalapi.ImageManagerService.
type remoteImageService struct {
	timeout     time.Duration
	imageClient runtimeapi.ImageServiceClient
}

// NewRemoteImageService creates a new internalapi.ImageManagerService.
func NewRemoteImageService(endpoint string, connectionTimeout time.Duration, tp trace.TracerProvider) (internalapi.ImageManagerService, error) {
	klog.V(3).InfoS("Connecting to image service", "endpoint", endpoint)
	addr, dialer, err := util.GetAddressAndDialer(endpoint)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectionTimeout)
	defer cancel()

	dialOpts := []grpc.DialOption{}
	dialOpts = append(dialOpts,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)))
	if utilfeature.DefaultFeatureGate.Enabled(features.KubeletTracing) {
		tracingOpts := []otelgrpc.Option{
			otelgrpc.WithPropagators(tracing.Propagators()),
			otelgrpc.WithTracerProvider(tp),
		}
		// Even if there is no TracerProvider, the otelgrpc still handles context propagation.
		// See https://github.com/open-telemetry/opentelemetry-go/tree/main/example/passthrough
		dialOpts = append(dialOpts,
			grpc.WithUnaryInterceptor(otelgrpc.UnaryClientInterceptor(tracingOpts...)),
			grpc.WithStreamInterceptor(otelgrpc.StreamClientInterceptor(tracingOpts...)))
	}

	conn, err := grpc.DialContext(ctx, addr, dialOpts...)
	if err != nil {
		klog.ErrorS(err, "Connect remote image service failed", "address", addr)
		return nil, err
	}

	service := &remoteImageService{timeout: connectionTimeout}

	if err := service.validateServiceConnection(conn, endpoint); err != nil {
		return nil, fmt.Errorf("validate service connection: %w", err)
	}

	return service, nil

}

// validateServiceConnection tries to connect to the remote image service by
// using the CRI v1 API version and fails if that's not possible.
func (r *remoteImageService) validateServiceConnection(conn *grpc.ClientConn, endpoint string) error {
	ctx, cancel := getContextWithTimeout(r.timeout)
	defer cancel()

	klog.V(4).InfoS("Validating the CRI v1 API image version")
	r.imageClient = runtimeapi.NewImageServiceClient(conn)

	if _, err := r.imageClient.ImageFsInfo(ctx, &runtimeapi.ImageFsInfoRequest{}); err == nil {
		klog.V(2).InfoS("Validated CRI v1 image API")

	} else if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("CRI v1 image API is not implemented for endpoint %q: %w", endpoint, err)
	}

	return nil
}

// ListImages lists available images.
func (r *remoteImageService) ListImages(filter *runtimeapi.ImageFilter) ([]*runtimeapi.Image, error) {
	ctx, cancel := getContextWithTimeout(r.timeout)
	defer cancel()

	return r.listImagesV1(ctx, filter)
}

func (r *remoteImageService) listImagesV1(ctx context.Context, filter *runtimeapi.ImageFilter) ([]*runtimeapi.Image, error) {
	resp, err := r.imageClient.ListImages(ctx, &runtimeapi.ListImagesRequest{
		Filter: filter,
	})
	if err != nil {
		klog.ErrorS(err, "ListImages with filter from image service failed", "filter", filter)
		return nil, err
	}

	return resp.Images, nil
}

// ImageStatus returns the status of the image.
func (r *remoteImageService) ImageStatus(image *runtimeapi.ImageSpec, verbose bool) (*runtimeapi.ImageStatusResponse, error) {
	ctx, cancel := getContextWithTimeout(r.timeout)
	defer cancel()

	return r.imageStatusV1(ctx, image, verbose)
}

func (r *remoteImageService) imageStatusV1(ctx context.Context, image *runtimeapi.ImageSpec, verbose bool) (*runtimeapi.ImageStatusResponse, error) {
	resp, err := r.imageClient.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{
		Image:   image,
		Verbose: verbose,
	})
	if err != nil {
		klog.ErrorS(err, "Get ImageStatus from image service failed", "image", image.Image)
		return nil, err
	}

	if resp.Image != nil {
		if resp.Image.Id == "" || resp.Image.Size_ == 0 {
			errorMessage := fmt.Sprintf("Id or size of image %q is not set", image.Image)
			err := errors.New(errorMessage)
			klog.ErrorS(err, "ImageStatus failed", "image", image.Image)
			return nil, err
		}
	}

	return resp, nil
}

// PullImage pulls an image with authentication config.
func (r *remoteImageService) PullImage(image *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	ctx, cancel := getContextWithCancel()
	defer cancel()

	return r.pullImageV1(ctx, image, auth, podSandboxConfig)
}

func (r *remoteImageService) pullImageV1(ctx context.Context, image *runtimeapi.ImageSpec, auth *runtimeapi.AuthConfig, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	resp, err := r.imageClient.PullImage(ctx, &runtimeapi.PullImageRequest{
		Image:         image,
		Auth:          auth,
		SandboxConfig: podSandboxConfig,
	})
	if err != nil {
		klog.ErrorS(err, "PullImage from image service failed", "image", image.Image)
		return "", err
	}

	if resp.ImageRef == "" {
		klog.ErrorS(errors.New("PullImage failed"), "ImageRef of image is not set", "image", image.Image)
		errorMessage := fmt.Sprintf("imageRef of image %q is not set", image.Image)
		return "", errors.New(errorMessage)
	}

	return resp.ImageRef, nil
}

// RemoveImage removes the image.
func (r *remoteImageService) RemoveImage(image *runtimeapi.ImageSpec) (err error) {
	ctx, cancel := getContextWithTimeout(r.timeout)
	defer cancel()

	if _, err = r.imageClient.RemoveImage(ctx, &runtimeapi.RemoveImageRequest{
		Image: image,
	}); err != nil {
		klog.ErrorS(err, "RemoveImage from image service failed", "image", image.Image)
		return err
	}

	return nil
}

// ImageFsInfo returns information of the filesystem that is used to store images.
func (r *remoteImageService) ImageFsInfo() ([]*runtimeapi.FilesystemUsage, error) {
	// Do not set timeout, because `ImageFsInfo` takes time.
	// TODO(random-liu): Should we assume runtime should cache the result, and set timeout here?
	ctx, cancel := getContextWithCancel()
	defer cancel()

	return r.imageFsInfoV1(ctx)
}

func (r *remoteImageService) imageFsInfoV1(ctx context.Context) ([]*runtimeapi.FilesystemUsage, error) {
	resp, err := r.imageClient.ImageFsInfo(ctx, &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		klog.ErrorS(err, "ImageFsInfo from image service failed")
		return nil, err
	}
	return resp.GetImageFilesystems(), nil
}
