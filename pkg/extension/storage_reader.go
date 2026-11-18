package extension

import (
	"context"
	"errors"
)

type StorageNativeAccess struct {
	Lease      StorageReadLease `json:"lease"`
	Descriptor int              `json:"descriptor"`
	Family     StorageFamily    `json:"family"`
}

type StorageFilesRequest struct {
	Lease     StorageReadLease `json:"lease"`
	Directory string           `json:"directory"`
	Cursor    string           `json:"cursor,omitempty"`
	Limit     int              `json:"limit"`
}

type StorageFileEntry struct {
	FileRef      string `json:"fileRef"`
	RelativeName string `json:"relativeName"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size"`
}

type StorageDirectoryPage struct {
	SnapshotRef string             `json:"snapshotRef"`
	Entries     []StorageFileEntry `json:"entries"`
	NextCursor  string             `json:"nextCursor,omitempty"`
	Missing     bool               `json:"missing,omitempty"`
}

type StorageBytesRequest struct {
	Lease   StorageReadLease `json:"lease"`
	FileRef string           `json:"fileRef"`
	Offset  int64            `json:"offset"`
	Length  int              `json:"length"`
}

type StorageReadChunk struct {
	SnapshotRef string `json:"snapshotRef"`
	FileRef     string `json:"fileRef"`
	Offset      int64  `json:"offset"`
	Bytes       []byte `json:"bytes"`
	EOF         bool   `json:"eof"`
}

func (stream *Stream) storageParent(lease StorageReadLease) error {
	call := stream.state.call
	request, err := DecodeStorageRequest(stream.server.manifest, call.Method, call.Scope, call.Auth, call.Params)
	if err != nil || request.Lease != lease {
		return errors.New("storage callbacks require their exact parent source lease")
	}
	return nil
}

func (stream *Stream) CheckStorage(ctx context.Context, lease StorageReadLease) error {
	if err := stream.storageParent(lease); err != nil {
		return err
	}
	return stream.HostCall(ctx, MethodStorageCheck, lease, nil)
}

func (stream *Stream) NativeStorage(ctx context.Context, lease StorageReadLease) (StorageNativeAccess, error) {
	var access StorageNativeAccess
	if err := stream.storageParent(lease); err != nil {
		return access, err
	}
	if err := stream.HostCall(ctx, MethodStorageNative, lease, &access); err != nil {
		return access, err
	}
	if access.Lease != lease || access.Descriptor < 0 {
		return StorageNativeAccess{}, errors.New("invalid native storage root lease")
	}
	return access, nil
}

func (stream *Stream) StorageFiles(ctx context.Context, request StorageFilesRequest) (StorageDirectoryPage, error) {
	var result StorageDirectoryPage
	if err := stream.storageParent(request.Lease); err != nil {
		return result, err
	}
	err := stream.HostCall(ctx, MethodStorageFiles, request, &result)
	if err == nil && (result.SnapshotRef != request.Lease.SnapshotRef || len(result.Entries) > request.Limit) {
		err = errors.New("invalid storage directory page")
	}
	return result, err
}

func (stream *Stream) StorageBytes(ctx context.Context, request StorageBytesRequest) (StorageReadChunk, error) {
	var result StorageReadChunk
	if err := stream.storageParent(request.Lease); err != nil {
		return result, err
	}
	err := stream.HostCall(ctx, MethodStorageBytes, request, &result)
	if err == nil && (result.SnapshotRef != request.Lease.SnapshotRef || result.FileRef != request.FileRef ||
		result.Offset != request.Offset || len(result.Bytes) > request.Length) {
		err = errors.New("invalid storage byte chunk")
	}
	return result, err
}
