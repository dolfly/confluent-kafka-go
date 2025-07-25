/**
 * Copyright 2024 Confluent Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package encryption

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/rest"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/rules/encryption/deks"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry/serde"
	"github.com/tink-crypto/tink-go/v2/aead"
	"github.com/tink-crypto/tink-go/v2/core/registry"
	"github.com/tink-crypto/tink-go/v2/daead"
	tinkpb "github.com/tink-crypto/tink-go/v2/proto/tink_go_proto"
	"github.com/tink-crypto/tink-go/v2/tink"
	"log"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register()
}

// Register registers the encryption rule executor
func Register() {
	serde.RegisterRuleExecutor(NewExecutor())
	serde.RegisterRuleExecutor(NewFieldExecutor())
}

// RegisterExecutorWithClock registers the encryption rule executor with a given clock
func RegisterExecutorWithClock(c Clock) *Executor {
	f := NewExecutorWithClock(c)
	serde.RegisterRuleExecutor(f)
	return f
}

// NewExecutor creates a new encryption rule executor
func NewExecutor() serde.RuleExecutor {
	c := clock{}
	return NewExecutorWithClock(&c)
}

// NewExecutorWithClock creates a new encryption rule executor with a given clock
func NewExecutorWithClock(c Clock) *Executor {
	f := &Executor{nil, nil, c}
	return f
}

const (
	// EncryptKekName represents a kek name
	EncryptKekName = "encrypt.kek.name"
	// EncryptKmsKeyID represents a kms key ID
	EncryptKmsKeyID = "encrypt.kms.key.id"
	// EncryptKmsType represents a kms type
	EncryptKmsType = "encrypt.kms.type"
	// EncryptDekAlgorithm represents a dek algorithm
	EncryptDekAlgorithm = "encrypt.dek.algorithm"
	// EncryptDekExpiryDays represents dek expiry days
	EncryptDekExpiryDays = "encrypt.dek.expiry.days"

	// Aes128Gcm represents AES128_GCM algorithm
	Aes128Gcm = "AES128_GCM"
	// Aes256Gcm represents AES256_GCM algorithm
	Aes256Gcm = "AES256_GCM"
	// Aes256Siv represents AES256_SIV algorithm
	Aes256Siv = "AES256_SIV"

	// MillisInDay represents number of milliseconds in a day
	MillisInDay = 24 * 60 * 60 * 1000
)

// Clock is a clock
type Clock interface {
	NowUnixMilli() int64
}

type clock struct{}

func (*clock) NowUnixMilli() int64 {
	return time.Now().UnixMilli()
}

// Executor is an encryption executor
type Executor struct {
	Config map[string]string
	Client deks.Client
	Clock  Clock
}

// Configure configures the executor
func (f *Executor) Configure(clientConfig *schemaregistry.Config, config map[string]string) error {
	if f.Client != nil {
		if !schemaregistry.ConfigsEqual(f.Client.Config(), clientConfig) {
			return errors.New("executor already configured")
		}
	} else {
		client, err := deks.NewClient(clientConfig)
		if err != nil {
			return err
		}
		f.Client = client
	}

	if f.Config != nil {
		for key, value := range config {
			v, ok := f.Config[key]
			if ok {
				if v != value {
					return fmt.Errorf("rule config key already set: %s", key)
				}
			} else {
				f.Config[key] = value
			}
		}
	} else if config != nil {
		f.Config = config
	} else {
		f.Config = make(map[string]string)
	}
	return nil
}

// Type returns the type of the executor
func (f *Executor) Type() string {
	return "ENCRYPT_PAYLOAD"
}

// Transform transforms the message using the rule
func (f *Executor) Transform(ctx serde.RuleContext, msg interface{}) (interface{}, error) {
	transform, err := f.NewTransform(ctx)
	if err != nil {
		return nil, err
	}
	return transform.Transform(ctx, serde.TypeBytes, msg)
}

// NewTransform creates a new transform
func (f *Executor) NewTransform(ctx serde.RuleContext) (*ExecutorTransform, error) {
	kekName, err := getKekName(ctx)
	if err != nil {
		return nil, err
	}
	dekExpiryDays, err := getDekExpiryDays(ctx)
	if err != nil {
		return nil, err
	}
	transform := ExecutorTransform{
		Executor:      *f,
		Cryptor:       getCryptor(ctx),
		KekName:       kekName,
		DekExpiryDays: dekExpiryDays,
	}
	kek, err := transform.getOrCreateKek(ctx)
	if err != nil {
		return nil, err
	}
	transform.Kek = *kek
	return &transform, nil
}

// Close closes the executor
func (f *Executor) Close() error {
	return f.Client.Close()
}

// ExecutorTransform is a field encryption executor transform
type ExecutorTransform struct {
	Executor      Executor
	Cryptor       Cryptor
	KekName       string
	Kek           deks.Kek
	DekExpiryDays int
}

// Cryptor is a cryptor
type Cryptor struct {
	DekFormat   string
	KeyTemplate *tinkpb.KeyTemplate
}

func getCryptor(ctx serde.RuleContext) Cryptor {
	algorithm := ctx.GetParameter(EncryptDekAlgorithm)
	if algorithm == nil {
		alg := Aes256Gcm
		algorithm = &alg
	}
	var keyTemplate *tinkpb.KeyTemplate
	switch *algorithm {
	case Aes128Gcm:
		keyTemplate = aead.AES128GCMKeyTemplate()
	case Aes256Gcm:
		keyTemplate = aead.AES256GCMKeyTemplate()
	case Aes256Siv:
		keyTemplate = daead.AESSIVKeyTemplate()
	}
	return Cryptor{
		DekFormat:   *algorithm,
		KeyTemplate: keyTemplate,
	}
}

func (c *Cryptor) encrypt(dek []byte, plaintext []byte, associatedData []byte) ([]byte, error) {
	primitive, err := registry.Primitive(c.KeyTemplate.TypeUrl, dek)
	if err != nil {
		return nil, err
	}
	switch c.DekFormat {
	case Aes256Siv:
		primitive := primitive.(tink.DeterministicAEAD)
		return primitive.EncryptDeterministically(plaintext, associatedData)
	default:
		primitive := primitive.(tink.AEAD)
		return primitive.Encrypt(plaintext, associatedData)
	}
}

func (c *Cryptor) decrypt(dek []byte, ciphertext []byte, associatedData []byte) ([]byte, error) {
	primitive, err := registry.Primitive(c.KeyTemplate.TypeUrl, dek)
	if err != nil {
		return nil, err
	}
	switch c.DekFormat {
	case Aes256Siv:
		primitive := primitive.(tink.DeterministicAEAD)
		return primitive.DecryptDeterministically(ciphertext, associatedData)
	default:
		primitive := primitive.(tink.AEAD)
		return primitive.Decrypt(ciphertext, associatedData)
	}
}

func toBytes(fieldType serde.FieldType, obj interface{}) []byte {
	switch fieldType {
	case serde.TypeBytes:
		return obj.([]byte)
	case serde.TypeString:
		return []byte(obj.(string))
	default:
		return nil
	}
}

func toObject(fieldType serde.FieldType, obj []byte) interface{} {
	switch fieldType {
	case serde.TypeBytes:
		return obj
	case serde.TypeString:
		return string(obj)
	default:
		return nil
	}
}

func getKekName(ctx serde.RuleContext) (string, error) {
	kekName := ctx.GetParameter(EncryptKekName)
	if kekName == nil {
		return "", errors.New("no kek name found")
	}
	if len(*kekName) == 0 {
		return "", errors.New("empty kek name")
	}
	return *kekName, nil
}

func getDekExpiryDays(ctx serde.RuleContext) (int, error) {
	dekExpiryDays := ctx.GetParameter(EncryptDekExpiryDays)
	if dekExpiryDays == nil {
		return 0, nil
	}
	i, err := strconv.Atoi(*dekExpiryDays)
	if err != nil {
		return -1, fmt.Errorf("invalid value for %s: %s", EncryptDekExpiryDays, *dekExpiryDays)
	}
	if i < 0 {
		return -1, fmt.Errorf("invalid value for %s: %s", EncryptDekExpiryDays, *dekExpiryDays)
	}
	return i, nil
}

func (f *ExecutorTransform) isDekRotated() bool {
	return f.DekExpiryDays > 0
}

func (f *ExecutorTransform) getOrCreateKek(ctx serde.RuleContext) (*deks.Kek, error) {
	isRead := ctx.RuleMode == schemaregistry.Read
	kekID := deks.KekID{
		Name:    f.KekName,
		Deleted: isRead,
	}
	kmsType := ctx.GetParameter(EncryptKmsType)
	kmsKeyID := ctx.GetParameter(EncryptKmsKeyID)
	kek, err := f.retrieveKekFromRegistry(kekID)
	if kek == nil {
		if isRead {
			return nil, fmt.Errorf("no kek found for %s during consume", f.KekName)
		}
		if kmsType == nil || len(*kmsType) == 0 {
			return nil, fmt.Errorf("no kms type found for %s during produce", f.KekName)
		}
		if kmsKeyID == nil || len(*kmsKeyID) == 0 {
			return nil, fmt.Errorf("no kms key id found for %s during produce", f.KekName)
		}
		kek, err = f.storeKekToRegistry(kekID, *kmsType, *kmsKeyID, false)
		if kek == nil {
			// Handle conflicts (409)
			kek, err = f.retrieveKekFromRegistry(kekID)
			if err != nil {
				return nil, err
			}
		}
		if kek == nil {
			return nil, fmt.Errorf("no kek found for %s during produce", f.KekName)
		}
	}
	if kmsType != nil && len(*kmsType) != 0 && *kmsType != kek.KmsType {
		return nil, fmt.Errorf("found %s with kms type %s which differs from rule kms type %s", f.KekName, kek.KmsType, *kmsType)
	}
	if kmsKeyID != nil && len(*kmsKeyID) != 0 && *kmsKeyID != kek.KmsKeyID {
		return nil, fmt.Errorf("found %s with kms key id %s which differs from rule kms key id %s", f.KekName, kek.KmsKeyID, *kmsKeyID)
	}
	return kek, nil
}

func (f *ExecutorTransform) retrieveKekFromRegistry(key deks.KekID) (*deks.Kek, error) {
	kek, err := f.Executor.Client.GetKek(key.Name, key.Deleted)
	if err != nil {
		var restErr *rest.Error
		if errors.As(err, &restErr) {
			if strings.HasPrefix(strconv.Itoa(restErr.Code), "404") {
				return nil, nil
			}
		}
		return nil, err
	}
	return &kek, nil
}

func (f *ExecutorTransform) storeKekToRegistry(key deks.KekID, kmsType string, kmsKeyID string, shared bool) (*deks.Kek, error) {
	kek, err := f.Executor.Client.RegisterKek(key.Name, kmsType, kmsKeyID, nil, "", shared)
	if err != nil {
		var restErr *rest.Error
		if errors.As(err, &restErr) {
			if strings.HasPrefix(strconv.Itoa(restErr.Code), "409") {
				return nil, nil
			}
		}
		return nil, err
	}
	return &kek, nil
}

func (f *ExecutorTransform) getOrCreateDek(ctx serde.RuleContext, version *int) (*deks.Dek, error) {
	isRead := ctx.RuleMode == schemaregistry.Read
	ver := 1
	if version != nil {
		ver = *version
	}
	dekID := deks.DekID{
		KekName:   f.KekName,
		Subject:   ctx.Subject,
		Version:   ver,
		Algorithm: f.Cryptor.DekFormat,
		Deleted:   isRead,
	}
	var primitive tink.AEAD
	dek, err := f.retrieveDekFromRegistry(dekID)
	if err != nil {
		return nil, err
	}
	isExpired := f.isExpired(ctx, dek)
	if dek == nil || isExpired {
		if isRead {
			return nil, fmt.Errorf("no dek found for %s during consumer", f.KekName)
		}
		var encryptedDek []byte
		if !f.Kek.Shared {
			primitive, err = getAead(f.Executor.Config, f.Kek)
			if err != nil {
				return nil, err
			}
			// Generate new dek
			keyData, err := registry.NewKeyData(f.Cryptor.KeyTemplate)
			if err != nil {
				return nil, err
			}
			rawDek := keyData.GetValue()
			encryptedDek, err = primitive.Encrypt(rawDek, []byte{})
			if err != nil {
				return nil, err
			}
		}
		newVersion := 1
		if isExpired {
			newVersion = dek.Version + 1
		}
		var result *deks.Dek
		result, err = f.createDek(dekID, newVersion, encryptedDek)
		if err != nil {
			if dek == nil {
				return nil, err
			}
			log.Printf("WARN: failed to create dek for %s, subject %s, version %d, using existing dek\n",
				f.KekName, ctx.Subject, newVersion)
		} else {
			dek = result
		}
	}
	keyBytes, err := f.Executor.Client.GetDekKeyMaterialBytes(dek)
	if err != nil {
		return nil, err
	}
	if keyBytes == nil {
		if primitive == nil {
			primitive, err = getAead(f.Executor.Config, f.Kek)
			if err != nil {
				return nil, err
			}
		}
		encryptedDek, err := f.Executor.Client.GetDekEncryptedKeyMaterialBytes(dek)
		if err != nil {
			return nil, err
		}
		rawDek, err := primitive.Decrypt(encryptedDek, []byte{})
		if err != nil {
			return nil, err
		}
		f.Executor.Client.SetDekKeyMaterial(dek, rawDek)
	}
	return dek, nil
}

func (f *ExecutorTransform) createDek(dekID deks.DekID, newVersion int, encryptedDek []byte) (*deks.Dek, error) {
	newDekID := deks.DekID{
		KekName:   dekID.KekName,
		Subject:   dekID.Subject,
		Version:   newVersion,
		Algorithm: dekID.Algorithm,
		Deleted:   dekID.Deleted,
	}
	// encryptedDek may be passed as null if kek is shared
	dek, err := f.storeDekToRegistry(newDekID, encryptedDek)
	if dek == nil {
		// Handle conflicts (409)
		// Use the original version, which should be null or LATEST_VERSION
		dek, err = f.retrieveDekFromRegistry(dekID)
		if err != nil {
			return nil, err
		}
	}
	if dek == nil {
		return nil, fmt.Errorf("no dek found for %s during produce", dekID.KekName)
	}
	return dek, nil
}

func (f *ExecutorTransform) retrieveDekFromRegistry(key deks.DekID) (*deks.Dek, error) {
	var dek deks.Dek
	var err error
	if key.Version != 0 {
		dek, err = f.Executor.Client.GetDekVersion(key.KekName, key.Subject, key.Version, key.Algorithm, key.Deleted)
	} else {
		dek, err = f.Executor.Client.GetDek(key.KekName, key.Subject, key.Algorithm, key.Deleted)
	}
	if err != nil {
		var restErr *rest.Error
		if errors.As(err, &restErr) {
			if strings.HasPrefix(strconv.Itoa(restErr.Code), "404") {
				return nil, nil
			}
		}
		return nil, err
	}
	return &dek, nil
}

func (f *ExecutorTransform) storeDekToRegistry(key deks.DekID, encryptedDek []byte) (*deks.Dek, error) {
	var encryptedDekStr string
	if encryptedDek != nil {
		encryptedDekStr = base64.StdEncoding.EncodeToString(encryptedDek)
	}
	var dek deks.Dek
	var err error
	if key.Version != 0 {
		dek, err = f.Executor.Client.RegisterDekVersion(key.KekName, key.Subject, key.Version, key.Algorithm, encryptedDekStr)
	} else {
		dek, err = f.Executor.Client.RegisterDek(key.KekName, key.Subject, key.Algorithm, encryptedDekStr)
	}
	if err != nil {
		var restErr *rest.Error
		if errors.As(err, &restErr) {
			if strings.HasPrefix(strconv.Itoa(restErr.Code), "409") {
				return nil, nil
			}
		}
		return nil, err
	}
	return &dek, nil
}

func (f *ExecutorTransform) isExpired(ctx serde.RuleContext, dek *deks.Dek) bool {
	now := f.Executor.Clock.NowUnixMilli()
	return ctx.RuleMode != schemaregistry.Read &&
		f.DekExpiryDays > 0 &&
		dek != nil &&
		(now-dek.Ts)/MillisInDay >= int64(f.DekExpiryDays)
}

// Transform transforms the field value using the rule
func (f *ExecutorTransform) Transform(ctx serde.RuleContext, fieldType serde.FieldType, fieldValue interface{}) (interface{}, error) {
	if fieldValue == nil {
		return nil, nil
	}
	switch ctx.RuleMode {
	case schemaregistry.Write:
		plaintext := toBytes(fieldType, fieldValue)
		if plaintext == nil {
			return nil, fmt.Errorf("type '%v' not supported for encryption", fieldType)
		}
		var version *int
		if f.isDekRotated() {
			v := -1
			version = &v
		}
		dek, err := f.getOrCreateDek(ctx, version)
		if err != nil {
			return nil, err
		}
		keyMaterialBytes, err := f.Executor.Client.GetDekKeyMaterialBytes(dek)
		if err != nil {
			return nil, err
		}
		ciphertext, err := f.Cryptor.encrypt(keyMaterialBytes, plaintext, []byte{})
		if err != nil {
			return nil, err
		}
		if f.isDekRotated() {
			ciphertext, err = prefixVersion(dek.Version, ciphertext)
			if err != nil {
				return nil, err
			}
		}
		if fieldType == serde.TypeString {
			return base64.StdEncoding.EncodeToString(ciphertext), nil
		}
		return ciphertext, nil
	case schemaregistry.Read:
		ciphertext := toBytes(fieldType, fieldValue)
		if ciphertext == nil {
			return fieldValue, nil
		}
		if fieldType == serde.TypeString {
			var err error
			ciphertext, err = base64.StdEncoding.DecodeString(string(ciphertext))
			if err != nil {
				return nil, err
			}
		}
		var version *int
		if f.isDekRotated() {
			v, err := extractVersion(ciphertext)
			if err != nil {
				return nil, err
			}
			version = &v
			ciphertext = ciphertext[5:]
		}
		dek, err := f.getOrCreateDek(ctx, version)
		if err != nil {
			return nil, err
		}
		keyMaterialBytes, err := f.Executor.Client.GetDekKeyMaterialBytes(dek)
		if err != nil {
			return nil, err
		}
		plaintext, err := f.Cryptor.decrypt(keyMaterialBytes, ciphertext, []byte{})
		if err != nil {
			return nil, err
		}
		return toObject(fieldType, plaintext), nil
	default:
		return nil, fmt.Errorf("unsupported rule mode %v", ctx.RuleMode)
	}
}

func prefixVersion(version int, ciphertext []byte) ([]byte, error) {
	var buf bytes.Buffer
	err := buf.WriteByte(serde.MagicByteV0)
	if err != nil {
		return nil, err
	}
	versionBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(versionBytes, uint32(version))
	_, err = buf.Write(versionBytes)
	if err != nil {
		return nil, err
	}
	_, err = buf.Write(ciphertext)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func extractVersion(ciphertext []byte) (int, error) {
	if ciphertext[0] != serde.MagicByteV0 {
		return -1, fmt.Errorf("unknown magic byte")
	}
	version := binary.BigEndian.Uint32(ciphertext[1:5])
	return int(version), nil
}

func getAead(config map[string]string, kek deks.Kek) (tink.AEAD, error) {
	kekURL := kek.KmsType + "://" + kek.KmsKeyID
	kmsClient, err := getKMSClient(config, kekURL)
	if err != nil {
		return nil, err
	}
	return kmsClient.GetAEAD(kekURL)
}

func getKMSClient(config map[string]string, kekURL string) (registry.KMSClient, error) {
	driver, err := GetKMSDriver(kekURL)
	if err != nil {
		return nil, err
	}
	client, err := registry.GetKMSClient(kekURL)
	if err != nil {
		client, err = registerKMSClient(driver, config, &kekURL)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	return client, nil
}

func registerKMSClient(kmsDriver KMSDriver, config map[string]string, keyURL *string) (registry.KMSClient, error) {
	kmsClient, err := kmsDriver.NewKMSClient(config, keyURL)
	if err != nil {
		return nil, err
	}
	registry.RegisterKMSClient(kmsClient)
	return kmsClient, nil
}

// KMSDriver is a KMS driver
type KMSDriver interface {
	GetKeyURLPrefix() string
	NewKMSClient(config map[string]string, keyURL *string) (registry.KMSClient, error)
}
