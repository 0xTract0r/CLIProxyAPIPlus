package auth

import "errors"

var (
	ErrAuthUploadLocalPending      = errors.New("credential upload is committed remotely; retry the same file to finish local installation")
	ErrAuthUploadRemoteUnconfirmed = errors.New("credential upload was not confirmed by the remote store")
	ErrAuthUploadPayloadConflict   = errors.New("a credential upload is pending; retry with the same file before changing this credential")
)
