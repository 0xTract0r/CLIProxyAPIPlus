//go:build !linux

package auth

import "os"

// Other platforms retain their existing save behavior until a native metadata
// check can be verified.
func guardOrdinaryCredentialMetadata(*os.File, string) (*ordinaryCredentialMetadataGuard, error) {
	return nil, nil
}
