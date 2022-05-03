// Copyright (c) 2022 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package manager

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/gardener/gardener/pkg/utils"
	secretutils "github.com/gardener/gardener/pkg/utils/secrets"

	"github.com/go-logr/logr"
	"github.com/mitchellh/hashstructure/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// LabelKeyName is a constant for a key of a label on a Secret describing the name.
	LabelKeyName = "name"
	// LabelKeyManagedBy is a constant for a key of a label on a Secret describing who is managing it.
	LabelKeyManagedBy = "managed-by"
	// LabelKeyManagerIdentity is a constant for a key of a label on a Secret describing which secret manager instance
	// is managing it.
	LabelKeyManagerIdentity = "manager-identity"
	// LabelKeyChecksumConfig is a constant for a key of a label on a Secret describing the checksum of the
	// configuration used to create the data.
	LabelKeyChecksumConfig = "checksum-of-config"
	// LabelKeyChecksumSigningCA is a constant for a key of a label on a Secret describing the checksum of the
	// certificate authority which has signed the client or server certificate in the data.
	LabelKeyChecksumSigningCA = "checksum-of-signing-ca"
	// LabelKeyBundleFor is a constant for a key of a label on a Secret describing that it is a bundle secret for
	// another secret.
	LabelKeyBundleFor = "bundle-for"
	// LabelKeyPersist is a constant for a key of a label on a Secret describing that it should get persisted.
	LabelKeyPersist = "persist"
	// LabelKeyLastRotationInitiationTime is a constant for a key of a label on a Secret describing the unix timestamps
	// of when the last secret rotation was initiated.
	LabelKeyLastRotationInitiationTime = "last-rotation-initiation-time"
	// LabelKeyIssuedAtTime is a constant for a key of a label on a Secret describing the time of when the secret data
	// was created. In case the data contains a certificate it is the time part of the certificate's 'not before' field.
	LabelKeyIssuedAtTime = "issued-at-time"
	// LabelKeyValidUntilTime is a constant for a key of a label on a Secret describing the time of how long the secret
	// data is valid. In case the data contains a certificate it is the time part of the certificate's 'not after'
	// field.
	LabelKeyValidUntilTime = "valid-until-time"

	// LabelValueTrue is a constant for a value of a label on a Secret describing the value 'true'.
	LabelValueTrue = "true"
	// LabelValueSecretsManager is a constant for a value of a label on a Secret describing the value 'secret-manager'.
	LabelValueSecretsManager = "secrets-manager"

	nameSuffixBundle = "-bundle"
)

type (
	manager struct {
		lock                        sync.Mutex
		clock                       clock.Clock
		store                       secretStore
		logger                      logr.Logger
		client                      client.Client
		namespace                   string
		identity                    string
		lastRotationInitiationTimes nameToUnixTime
	}

	nameToUnixTime map[string]string

	secretStore map[string]secretInfos
	secretInfos struct {
		current secretInfo
		old     *secretInfo
		bundle  *secretInfo
	}
	secretInfo struct {
		obj                        *corev1.Secret
		dataChecksum               string
		lastRotationInitiationTime int64
	}
)

var _ Interface = &manager{}

type secretClass string

const (
	current secretClass = "current"
	old     secretClass = "old"
	bundle  secretClass = "bundle"
)

// New returns a new manager for secrets in a given namespace.
func New(
	ctx context.Context,
	logger logr.Logger,
	clock clock.Clock,
	c client.Client,
	namespace string,
	identity string,
	secretNamesToTimes map[string]time.Time,
) (
	Interface,
	error,
) {
	m := &manager{
		store:                       make(secretStore),
		clock:                       clock,
		logger:                      logger.WithValues("namespace", namespace),
		client:                      c,
		namespace:                   namespace,
		identity:                    identity,
		lastRotationInitiationTimes: make(map[string]string),
	}

	if err := m.initialize(ctx, secretNamesToTimes); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *manager) listSecrets(ctx context.Context) (*corev1.SecretList, error) {
	secretList := &corev1.SecretList{}
	return secretList, m.client.List(ctx, secretList, client.InNamespace(m.namespace), client.MatchingLabels{
		LabelKeyManagedBy:       LabelValueSecretsManager,
		LabelKeyManagerIdentity: m.identity,
	})
}

func (m *manager) initialize(ctx context.Context, secretNamesToTimes map[string]time.Time) error {
	secretList, err := m.listSecrets(ctx)
	if err != nil {
		return err
	}

	nameToNewestSecret := make(map[string]corev1.Secret, len(secretList.Items))

	// Find the newest secret in system for the respective secret names. Read their existing
	// last-rotation-initiation-time labels and store them in our internal map.
	for _, secret := range secretList.Items {
		oldSecret, found := nameToNewestSecret[secret.Labels[LabelKeyName]]
		if !found || oldSecret.CreationTimestamp.Time.Before(secret.CreationTimestamp.Time) {
			nameToNewestSecret[secret.Labels[LabelKeyName]] = *secret.DeepCopy()
			m.lastRotationInitiationTimes[secret.Labels[LabelKeyName]] = secret.Labels[LabelKeyLastRotationInitiationTime]
		}
	}

	// Check if the secrets must be automatically renewed because they are about to expire.
	for name, secret := range nameToNewestSecret {
		mustRenew, err := m.mustAutoRenewSecret(secret)
		if err != nil {
			return err
		}

		if mustRenew {
			m.logger.Info("Preparing secret for automatic renewal", "secret", secret.Name, "issuedAt", secret.Labels[LabelKeyIssuedAtTime], "validUntil", secret.Labels[LabelKeyValidUntilTime])
			m.lastRotationInitiationTimes[name] = unixTime(m.clock.Now())
		}
	}

	// If the user has provided last rotation initiation times then use those.
	for name, time := range secretNamesToTimes {
		m.lastRotationInitiationTimes[name] = unixTime(time)
	}

	return nil
}

func (m *manager) mustAutoRenewSecret(secret corev1.Secret) (bool, error) {
	if secret.Labels[LabelKeyIssuedAtTime] == "" || secret.Labels[LabelKeyValidUntilTime] == "" {
		return false, nil
	}

	issuedAtUnix, err := strconv.ParseInt(secret.Labels[LabelKeyIssuedAtTime], 10, 64)
	if err != nil {
		return false, err
	}

	validUntilUnix, err := strconv.ParseInt(secret.Labels[LabelKeyValidUntilTime], 10, 64)
	if err != nil {
		return false, err
	}

	var (
		validity    = validUntilUnix - issuedAtUnix
		renewAtUnix = issuedAtUnix + validity*80/100
		renewAt     = time.Unix(renewAtUnix, 0).UTC()
		validUntil  = time.Unix(validUntilUnix, 0).UTC()
		now         = m.clock.Now().UTC()
	)

	// Renew if 80% of the validity has been reached or if the secret expires in less than 10d.
	return now.After(renewAt) || now.After(validUntil.Add(-10*24*time.Hour)), nil
}

func (m *manager) addToStore(name string, secret *corev1.Secret, class secretClass) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	info, err := computeSecretInfo(secret)
	if err != nil {
		return err
	}

	secrets := m.store[name]

	switch class {
	case current:
		secrets.current = info
	case old:
		secrets.old = &info
	case bundle:
		secrets.bundle = &info
	}

	m.store[name] = secrets

	return nil
}

func (m *manager) getFromStore(name string) (secretInfos, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	secrets, ok := m.store[name]
	return secrets, ok
}

func computeSecretInfo(obj *corev1.Secret) (secretInfo, error) {
	var (
		lastRotationStartTime int64
		err                   error
	)

	if v := obj.Labels[LabelKeyLastRotationInitiationTime]; len(v) > 0 {
		lastRotationStartTime, err = strconv.ParseInt(obj.Labels[LabelKeyLastRotationInitiationTime], 10, 64)
		if err != nil {
			return secretInfo{}, err
		}
	}

	return secretInfo{
		obj:                        obj,
		dataChecksum:               utils.ComputeSecretChecksum(obj.Data),
		lastRotationInitiationTime: lastRotationStartTime,
	}, nil
}

// ObjectMeta returns the object meta based on the given settings.
func ObjectMeta(
	namespace string,
	managerIdentity string,
	config secretutils.ConfigInterface,
	ignoreConfigChecksumForCASecretName bool,
	lastRotationInitiationTime string,
	validUntilTime *string,
	signingCAChecksum *string,
	persist *bool,
	bundleFor *string,
) (
	metav1.ObjectMeta,
	error,
) {
	configHash, err := hashstructure.Hash(config, hashstructure.FormatV2, &hashstructure.HashOptions{IgnoreZeroValue: true})
	if err != nil {
		return metav1.ObjectMeta{}, err
	}

	labels := map[string]string{
		LabelKeyName:                       config.GetName(),
		LabelKeyManagedBy:                  LabelValueSecretsManager,
		LabelKeyManagerIdentity:            managerIdentity,
		LabelKeyChecksumConfig:             strconv.FormatUint(configHash, 10),
		LabelKeyLastRotationInitiationTime: lastRotationInitiationTime,
	}

	if signingCAChecksum != nil {
		labels[LabelKeyChecksumSigningCA] = *signingCAChecksum
	}

	if validUntilTime != nil {
		labels[LabelKeyValidUntilTime] = *validUntilTime
	}

	if persist != nil && *persist {
		labels[LabelKeyPersist] = LabelValueTrue
	}

	if bundleFor != nil {
		labels[LabelKeyBundleFor] = *bundleFor
	}

	return metav1.ObjectMeta{
		Name:      computeSecretName(config, labels, ignoreConfigChecksumForCASecretName),
		Namespace: namespace,
		Labels:    labels,
	}, nil
}

func computeSecretName(config secretutils.ConfigInterface, labels map[string]string, ignoreConfigChecksumForCASecretName bool) string {
	name := config.GetName()

	// For backwards-compatibility, we might need to keep the static names of the CA secrets so that external components
	// (like extensions, etc.) relying on them don't break. This is why it is possible to opt out of the fact that the
	// config checksum is considered for the name computation.
	if cfg, ok := config.(*secretutils.CertificateSecretConfig); !ok || cfg.SigningCA != nil || !ignoreConfigChecksumForCASecretName {
		if infix := labels[LabelKeyChecksumConfig] + labels[LabelKeyChecksumSigningCA]; len(infix) > 0 {
			name += "-" + utils.ComputeSHA256Hex([]byte(infix))[:8]
		}
	}

	if suffix := labels[LabelKeyLastRotationInitiationTime]; len(suffix) > 0 {
		name += "-" + utils.ComputeSHA256Hex([]byte(suffix))[:5]
	}

	return name
}

// Secret constructs a *corev1.Secret for the given metadata and data.
func Secret(objectMeta metav1.ObjectMeta, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: objectMeta,
		Data:       data,
		Type:       secretTypeForData(data),
		Immutable:  pointer.Bool(true),
	}
}

func secretTypeForData(data map[string][]byte) corev1.SecretType {
	secretType := corev1.SecretTypeOpaque
	if data[secretutils.DataKeyCertificate] != nil && data[secretutils.DataKeyPrivateKey] != nil {
		secretType = corev1.SecretTypeTLS
	}
	return secretType
}

func unixTime(in time.Time) string {
	return strconv.FormatInt(in.UTC().Unix(), 10)
}
