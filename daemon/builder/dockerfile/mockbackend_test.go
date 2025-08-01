package dockerfile

import (
	"context"
	"encoding/json"
	"io"
	"runtime"

	"github.com/docker/docker/daemon/builder"
	"github.com/docker/docker/daemon/internal/image"
	"github.com/docker/docker/daemon/internal/layer"
	"github.com/docker/docker/daemon/server/backend"
	"github.com/moby/moby/api/types/container"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// MockBackend implements the builder.Backend interface for unit testing
type MockBackend struct {
	containerCreateFunc func(config backend.ContainerCreateConfig) (container.CreateResponse, error)
	commitFunc          func(backend.CommitConfig) (image.ID, error)
	getImageFunc        func(string) (builder.Image, builder.ROLayer, error)
	makeImageCacheFunc  func(cacheFrom []string) builder.ImageCache
}

func (m *MockBackend) ContainerAttachRaw(cID string, stdin io.ReadCloser, stdout, stderr io.Writer, stream bool, attached chan struct{}) error {
	return nil
}

func (m *MockBackend) ContainerCreateIgnoreImagesArgsEscaped(ctx context.Context, config backend.ContainerCreateConfig) (container.CreateResponse, error) {
	if m.containerCreateFunc != nil {
		return m.containerCreateFunc(config)
	}
	return container.CreateResponse{}, nil
}

func (m *MockBackend) ContainerRm(name string, config *backend.ContainerRmConfig) error {
	return nil
}

func (m *MockBackend) CommitBuildStep(ctx context.Context, c backend.CommitConfig) (image.ID, error) {
	if m.commitFunc != nil {
		return m.commitFunc(c)
	}
	return "", nil
}

func (m *MockBackend) ContainerStart(ctx context.Context, containerID string, checkpoint string, checkpointDir string) error {
	return nil
}

func (m *MockBackend) ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.StateStatus, error) {
	return nil, nil
}

func (m *MockBackend) ContainerCreateWorkdir(containerID string) error {
	return nil
}

func (m *MockBackend) CopyOnBuild(containerID string, destPath string, srcRoot string, srcPath string, decompress bool) error {
	return nil
}

func (m *MockBackend) GetImageAndReleasableLayer(ctx context.Context, refOrID string, opts backend.GetImageAndLayerOptions) (builder.Image, builder.ROLayer, error) {
	if m.getImageFunc != nil {
		return m.getImageFunc(refOrID)
	}

	return &mockImage{id: "theid"}, &mockLayer{}, nil
}

func (m *MockBackend) MakeImageCache(ctx context.Context, cacheFrom []string) (builder.ImageCache, error) {
	if m.makeImageCacheFunc != nil {
		return m.makeImageCacheFunc(cacheFrom), nil
	}
	return nil, nil
}

func (m *MockBackend) CreateImage(ctx context.Context, config []byte, parent string, layerDigest digest.Digest) (builder.Image, error) {
	return &mockImage{id: "test"}, nil
}

type mockImage struct {
	id     string
	config *container.Config
}

func (i *mockImage) ImageID() string {
	return i.id
}

func (i *mockImage) RunConfig() *container.Config {
	return i.config
}

func (i *mockImage) OperatingSystem() string {
	return runtime.GOOS
}

func (i *mockImage) MarshalJSON() ([]byte, error) {
	type rawImage mockImage
	return json.Marshal(rawImage(*i)) //nolint:staticcheck
}

type mockImageCache struct {
	getCacheFunc func(parentID string, cfg *container.Config) (string, error)
}

func (mic *mockImageCache) GetCache(parentID string, cfg *container.Config, _ ocispec.Platform) (string, error) {
	if mic.getCacheFunc != nil {
		return mic.getCacheFunc(parentID, cfg)
	}
	return "", nil
}

type mockLayer struct{}

func (l *mockLayer) ContentStoreDigest() digest.Digest {
	return ""
}

func (l *mockLayer) Release() error {
	return nil
}

func (l *mockLayer) NewRWLayer() (builder.RWLayer, error) {
	return &mockRWLayer{}, nil
}

func (l *mockLayer) DiffID() layer.DiffID {
	return "abcdef"
}

type mockRWLayer struct{}

func (l *mockRWLayer) Release() error {
	return nil
}

func (l *mockRWLayer) Commit() (builder.ROLayer, error) {
	return nil, nil
}

func (l *mockRWLayer) Root() string {
	return ""
}
