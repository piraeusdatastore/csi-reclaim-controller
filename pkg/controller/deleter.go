package controller

import (
	"context"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
)

// csiVolumeDeleter implements VolumeDeleter against a live CSI controller
// endpoint. DeleteVolume is required by the CSI spec to be idempotent, so
// deleting an already-gone volume is a no-op success.
type csiVolumeDeleter struct {
	client  csi.ControllerClient
	timeout time.Duration
}

// NewCSIVolumeDeleter returns a VolumeDeleter that calls DeleteVolume on the CSI
// controller reachable over conn. Each call is bounded by timeout.
func NewCSIVolumeDeleter(conn *grpc.ClientConn, timeout time.Duration) VolumeDeleter {
	return &csiVolumeDeleter{
		client:  csi.NewControllerClient(conn),
		timeout: timeout,
	}
}

func (d *csiVolumeDeleter) DeleteVolume(ctx context.Context, volumeID string, secrets map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	_, err := d.client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
		Secrets:  secrets,
	})
	return err
}
