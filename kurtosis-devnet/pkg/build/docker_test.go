package build

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDockerClient implements the dockerClient interface for testing
type mockDockerClient struct {
	inspectFunc func(ctx context.Context, imageID string) (types.ImageInspect, []byte, error)
	tagFunc     func(ctx context.Context, source, target string) error
}

func (m *mockDockerClient) ImageInspectWithRaw(ctx context.Context, imageID string) (types.ImageInspect, []byte, error) {
	return m.inspectFunc(ctx, imageID)
}

func (m *mockDockerClient) ImageTag(ctx context.Context, source, target string) error {
	return m.tagFunc(ctx, source, target)
}

// mockDockerProvider implements the dockerProvider interface for testing
type mockDockerProvider struct {
	client dockerClient
}

func (p *mockDockerProvider) newClient() (dockerClient, error) {
	return p.client, nil
}

// mockCmd is a mock for command execution that always succeeds
type mockCmd struct {
	output []byte
	dir    string
}

func (m *mockCmd) CombinedOutput() ([]byte, error) {
	return m.output, nil
}

func (m *mockCmd) Dir() string {
	return m.dir
}

func (m *mockCmd) SetDir(dir string) {
	m.dir = dir
}

// mockCmdFactory creates mock commands for testing
func mockCmdFactory(output []byte) cmdFactory {
	return func(name string, arg ...string) cmdRunner {
		return &mockCmd{output: output}
	}
}

// TestDockerBuilderNaming tests the image naming logic in the DockerBuilder
func TestDockerBuilderNaming(t *testing.T) {
	t.Run("successful_image_build_and_tag", func(t *testing.T) {
		mockClient := &mockDockerClient{
			inspectFunc: func(ctx context.Context, imageID string) (types.ImageInspect, []byte, error) {
				return types.ImageInspect{
					ID: "sha256:abcdef123456789abcdef123456789abcdef1234",
				}, nil, nil
			},
			tagFunc: func(ctx context.Context, source, target string) error {
				return nil
			},
		}
		mockProvider := &mockDockerProvider{client: mockClient}
		mockCmd := mockCmdFactory([]byte("test"))

		builder := NewDockerBuilder(
			withDockerProvider(mockProvider),
			withCmdFactory(mockCmd),
		)

		image, err := builder.Build("test-project", "test-image:latest")
		require.NoError(t, err)
		assert.Equal(t, "test-project:abcdef123456", image)
	})

	t.Run("tag_error", func(t *testing.T) {
		mockClient := &mockDockerClient{
			inspectFunc: func(ctx context.Context, imageID string) (types.ImageInspect, []byte, error) {
				return types.ImageInspect{
					ID: "sha256:abcdef123456789abcdef123456789abcdef1234",
				}, nil, nil
			},
			tagFunc: func(ctx context.Context, source, target string) error {
				return assert.AnError
			},
		}
		mockProvider := &mockDockerProvider{client: mockClient}
		mockCmd := mockCmdFactory([]byte("test"))

		builder := NewDockerBuilder(
			withDockerProvider(mockProvider),
			withCmdFactory(mockCmd),
		)

		_, err := builder.Build("test-project", "test-image:latest")
		require.Error(t, err)
		assert.Contains(t, err.Error(), assert.AnError.Error())
	})
}
