/*
Copyright 2022.

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

package validate

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"

	cliType "github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/kyma-project/warden/internal/helpers"
	"github.com/kyma-project/warden/pkg"
	"github.com/pkg/errors"
)

//go:generate mockery --name=ImageValidatorService
type ImageValidatorService interface {
	Validate(ctx context.Context, image string, imagePullCredentials map[string]cliType.AuthConfig) error
}

type ServiceConfig struct {
	NotaryConfig      NotaryConfig
	AllowedRegistries []string
}

type notaryService struct {
	ServiceConfig
	RepoFactory RepoFactory
}

func NewImageValidator(sc *ServiceConfig, notaryClientFactory RepoFactory) ImageValidatorService {
	return &notaryService{
		ServiceConfig: ServiceConfig{
			NotaryConfig:      sc.NotaryConfig,
			AllowedRegistries: sc.AllowedRegistries,
		},
		RepoFactory: notaryClientFactory,
	}
}

func (s *notaryService) Validate(ctx context.Context, image string, imagePullCredentials map[string]cliType.AuthConfig) error {
	logger := helpers.LoggerFromCtx(ctx).With("image", image)
	ctx = helpers.LoggerToContext(ctx, logger)

	if allowed := s.isImageAllowed(image); allowed {
		logger.Info("image validation skipped, because it's allowed")
		return nil
	}

	// strict validation requires image name to contain domain and a tag, and/or sha256
	ref, err := name.ParseReference(image, name.StrictValidation)
	if err != nil {
		return pkg.NewValidationFailedErr(errors.Wrap(err, "image name could not be parsed"))
	}

	expectedShaBytes, err := s.loggedGetNotaryImageDigestHash(ctx, ref)
	if err != nil {
		return err
	}

	shaImageBytes, shaManifestBytes, err := s.loggedGetRepositoryDigestHash(ctx, ref, imagePullCredentials)
	if err != nil {
		return err
	}

	if subtle.ConstantTimeCompare(shaImageBytes, expectedShaBytes) == 1 {
		return nil
	}

	if shaManifestBytes != nil && subtle.ConstantTimeCompare(shaManifestBytes, expectedShaBytes) == 1 {
		logger.Warn("deprecated: manifest hash was used for verification")
		return nil
	}

	return pkg.NewValidationFailedErr(errors.New("unexpected image hash value"))
}

func (s *notaryService) isImageAllowed(imgRepo string) bool {
	for _, allowed := range s.AllowedRegistries {
		// repository is in allowed list
		if strings.HasPrefix(imgRepo, allowed) {
			return true
		}
	}
	return false
}

func (s *notaryService) loggedGetRepositoryDigestHash(ctx context.Context, ref name.Reference, imagePullCredentials map[string]cliType.AuthConfig) ([]byte, []byte, error) {
	const message = "request to image registry"
	closeLog := helpers.LogStartTime(ctx, message)
	defer closeLog()
	return s.getRepositoryDigestHash(ref, imagePullCredentials)
}

func (s *notaryService) getRepositoryDigestHash(ref name.Reference, imagePullCredentials map[string]cliType.AuthConfig) ([]byte, []byte, error) {
	remoteOptions := make([]remote.Option, 0)

	credentials, credentialsOk := imagePullCredentials[ref.Context().RegistryStr()]

	//try to get image info without credentials, mimicking Kuberenetes behavior
	descriptor, err := remote.Get(ref)
	if err != nil {
		if !credentialsOk {
			// no fitting credentials, and no public access, return error
			return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "get image descriptor anonymously"))
		} else {
			// to to authenticate to the registry

			credentials, err := parseCredentials(credentials)
			if err != nil {
				return nil, nil, err
			}

			if credentials != nil {
				remoteOptions = append(remoteOptions, remote.WithAuth(credentials))
			}
			descriptor, err = remote.Get(ref, remoteOptions...)
			if err != nil {
				return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "get image descriptor"))
			}
		}
	}

	if descriptor.MediaType.IsIndex() {
		digest, err := getIndexDigestHash(ref, remoteOptions...)
		if err != nil {
			return nil, nil, err
		}
		return digest, nil, nil
	} else if descriptor.MediaType.IsImage() {
		digest, manifest, err := getImageDigestHash(ref, remoteOptions...)
		if err != nil {
			return nil, nil, err
		}
		return digest, manifest, nil
	}
	return nil, nil, pkg.NewValidationFailedErr(errors.New("not an image or image list"))
}

func parseCredentials(credentials cliType.AuthConfig) (authn.Authenticator, error) {
	if credentials.Username != "" && credentials.Password != "" {
		basicCredentials := &authn.Basic{Username: credentials.Username, Password: credentials.Password}
		return basicCredentials, nil
	} else if credentials.RegistryToken != "" {
		tokenCredentials := &authn.Bearer{Token: credentials.RegistryToken}
		return tokenCredentials, nil
	} else if credentials.Auth != "" {
		// auth is in base64-encoded "username:password" format
		decodedCredentials, err := base64.StdEncoding.DecodeString(credentials.Auth)
		if err != nil {
			return nil, pkg.NewValidationFailedErr(errors.Wrap(err, "cannot decode base64 encoded auth"))
		}

		auth := strings.Split(string(decodedCredentials), ":")
		if len(auth) != 2 {
			return nil, pkg.NewValidationFailedErr(errors.New("invalid auth format, expected username:password form"))
		}
		basicCredentials := &authn.Basic{Username: auth[0], Password: auth[1]}
		return basicCredentials, nil
	}
	return nil, pkg.NewValidationFailedErr(errors.New("unknown auth secret format"))
}

func getIndexDigestHash(ref name.Reference, remoteOptions ...remote.Option) ([]byte, error) {
	i, err := remote.Index(ref, remoteOptions...)
	if err != nil {
		return nil, pkg.NewUnknownResultErr(errors.Wrap(err, "get image"))
	}
	digest, err := i.Digest()
	if err != nil {
		return nil, pkg.NewUnknownResultErr(errors.Wrap(err, "image digest"))
	}
	digestBytes, err := hex.DecodeString(digest.Hex)
	if err != nil {
		return nil, pkg.NewUnknownResultErr(errors.Wrap(err, "checksum error: %w"))
	}
	return digestBytes, nil
}

func getImageDigestHash(ref name.Reference, remoteOptions ...remote.Option) ([]byte, []byte, error) {
	i, err := remote.Image(ref, remoteOptions...)
	if err != nil {
		return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "get image"))
	}

	// Deprecated: Remove manifest hash verification after all images has been signed using the new method
	m, err := i.Manifest()
	if err != nil {
		return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "image manifest"))
	}

	manifestBytes, err := hex.DecodeString(m.Config.Digest.Hex)
	if err != nil {
		return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "manifest checksum error: %w"))
	}

	digest, err := i.Digest()
	if err != nil {
		return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "image digest"))
	}

	digestBytes, err := hex.DecodeString(digest.Hex)

	if err != nil {
		return nil, nil, pkg.NewUnknownResultErr(errors.Wrap(err, "checksum error: %w"))
	}

	return digestBytes, manifestBytes, nil
}

func (s *notaryService) loggedGetNotaryImageDigestHash(ctx context.Context, ref name.Reference) ([]byte, error) {
	const message = "request to notary"
	closeLog := helpers.LogStartTime(ctx, message)
	defer closeLog()
	result, err := s.getNotaryImageDigestHash(ctx, ref)
	return result, err
}

func (s *notaryService) getNotaryImageDigestHash(ctx context.Context, ref name.Reference) ([]byte, error) {
	const messageNewRepoClient = "request to notary (NewRepoClient)"
	closeLog := helpers.LogStartTime(ctx, messageNewRepoClient)
	c, err := s.RepoFactory.NewRepoClient(ref.Context().Name(), s.NotaryConfig)
	closeLog()
	if err != nil {
		return nil, pkg.NewUnknownResultErr(err)
	}

	const messageGetTargetByName = "request to notary (GetTargetByName)"
	closeLog = helpers.LogStartTime(ctx, messageGetTargetByName)
	target, err := c.GetTargetByName(ref.Identifier())
	closeLog()
	if err != nil {
		return nil, parseNotaryErr(err)
	}

	if len(target.Hashes) == 0 {
		return nil, pkg.NewValidationFailedErr(errors.New("image hash is missing"))
	}

	if len(target.Hashes) > 1 {
		return nil, pkg.NewValidationFailedErr(errors.New("more than one hash for image"))
	}

	key := ""
	for i := range target.Hashes {
		key = i
	}

	return target.Hashes[key], nil
}

func parseNotaryErr(err error) error {
	errMsg := err.Error()
	if strings.Contains(errMsg, "does not have trust data for") {
		return pkg.NewValidationFailedErr(err)
	}
	if strings.Contains(errMsg, "No valid trust data for") {
		return pkg.NewValidationFailedErr(err)
	}
	return pkg.NewUnknownResultErr(err)
}
