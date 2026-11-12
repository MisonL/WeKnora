package types

import (
	"fmt"
	"log"

	"github.com/Tencent/WeKnora/internal/utils"
)

// encryptEngineCredential encrypts one credential field for at-rest storage.
// An unset SYSTEM_AES_KEY keeps the historical plaintext behavior (desktop and
// local development); a configured-but-broken key is a hard error so a write
// never silently stores a plaintext credential.
func encryptEngineCredential(label string, value *string) error {
	if value == nil || *value == "" {
		return nil
	}
	key := utils.GetAESKey()
	if key == nil {
		return nil
	}
	encrypted, err := utils.EncryptAESGCM(*value, key)
	if err != nil {
		return fmt.Errorf("encrypt %s: %w", label, err)
	}
	*value = encrypted
	return nil
}

// decryptEngineCredential is the lenient load-path counterpart: a row with
// broken ciphertext (key rotated/removed) must still load so the workspace can
// be inspected and re-credentialed. The field is cleared but its ciphertext is
// kept in preserved, so a later Value() can write it back instead of
// destroying a credential the original key may still recover.
func decryptEngineCredential(preserved preservedCiphertext, key, label string, value *string) {
	if value == nil || *value == "" {
		return
	}
	if plain, ok := utils.DecryptStoredSecretLenient(*value); ok {
		*value = plain
		return
	}
	log.Printf("[crypto] tenant %s: decrypt failed (SYSTEM_AES_KEY missing/rotated?), keeping ciphertext for write-back", label)
	preserved.remember(key, *value)
	*value = ""
}

// encryptStorageEngineCredentials encrypts every credential-bearing field of a
// copied StorageEngineConfig. The receiver is not mutated: provider structs are
// shallow-copied before their fields change.
func encryptStorageEngineCredentials(c *StorageEngineConfig) error {
	if c.MinIO != nil {
		m := *c.MinIO
		if err := encryptEngineCredential("storage minio access_key_id", &m.AccessKeyID); err != nil {
			return err
		}
		if err := encryptEngineCredential("storage minio secret_access_key", &m.SecretAccessKey); err != nil {
			return err
		}
		c.MinIO = &m
	}
	if c.COS != nil {
		m := *c.COS
		if err := encryptEngineCredential("storage cos secret_id", &m.SecretID); err != nil {
			return err
		}
		if err := encryptEngineCredential("storage cos secret_key", &m.SecretKey); err != nil {
			return err
		}
		c.COS = &m
	}
	encryptPair := func(label string, accessKey, secretKey *string) error {
		if err := encryptEngineCredential("storage "+label+" access_key", accessKey); err != nil {
			return err
		}
		return encryptEngineCredential("storage "+label+" secret_key", secretKey)
	}
	if c.TOS != nil {
		m := *c.TOS
		if err := encryptPair("tos", &m.AccessKey, &m.SecretKey); err != nil {
			return err
		}
		c.TOS = &m
	}
	if c.S3 != nil {
		m := *c.S3
		if err := encryptPair("s3", &m.AccessKey, &m.SecretKey); err != nil {
			return err
		}
		c.S3 = &m
	}
	if c.OSS != nil {
		m := *c.OSS
		if err := encryptPair("oss", &m.AccessKey, &m.SecretKey); err != nil {
			return err
		}
		c.OSS = &m
	}
	if c.KS3 != nil {
		m := *c.KS3
		if err := encryptPair("ks3", &m.AccessKey, &m.SecretKey); err != nil {
			return err
		}
		c.KS3 = &m
	}
	if c.OBS != nil {
		m := *c.OBS
		if err := encryptPair("obs", &m.AccessKey, &m.SecretKey); err != nil {
			return err
		}
		c.OBS = &m
	}
	return nil
}

// decryptStorageEngineCredentials reverses encryptStorageEngineCredentials in
// place on a freshly unmarshalled value.
func decryptStorageEngineCredentials(c *StorageEngineConfig) {
	c.undecryptable = newPreservedCiphertext()
	if c.MinIO != nil {
		decryptEngineCredential(c.undecryptable, "minio.access_key_id", "storage minio access_key_id", &c.MinIO.AccessKeyID)
		decryptEngineCredential(c.undecryptable, "minio.secret_access_key", "storage minio secret_access_key", &c.MinIO.SecretAccessKey)
	}
	if c.COS != nil {
		decryptEngineCredential(c.undecryptable, "cos.secret_id", "storage cos secret_id", &c.COS.SecretID)
		decryptEngineCredential(c.undecryptable, "cos.secret_key", "storage cos secret_key", &c.COS.SecretKey)
	}
	if c.TOS != nil {
		decryptEngineCredential(c.undecryptable, "tos.access_key", "storage tos access_key", &c.TOS.AccessKey)
		decryptEngineCredential(c.undecryptable, "tos.secret_key", "storage tos secret_key", &c.TOS.SecretKey)
	}
	if c.S3 != nil {
		decryptEngineCredential(c.undecryptable, "s3.access_key", "storage s3 access_key", &c.S3.AccessKey)
		decryptEngineCredential(c.undecryptable, "s3.secret_key", "storage s3 secret_key", &c.S3.SecretKey)
	}
	if c.OSS != nil {
		decryptEngineCredential(c.undecryptable, "oss.access_key", "storage oss access_key", &c.OSS.AccessKey)
		decryptEngineCredential(c.undecryptable, "oss.secret_key", "storage oss secret_key", &c.OSS.SecretKey)
	}
	if c.KS3 != nil {
		decryptEngineCredential(c.undecryptable, "ks3.access_key", "storage ks3 access_key", &c.KS3.AccessKey)
		decryptEngineCredential(c.undecryptable, "ks3.secret_key", "storage ks3 secret_key", &c.KS3.SecretKey)
	}
	if c.OBS != nil {
		decryptEngineCredential(c.undecryptable, "obs.access_key", "storage obs access_key", &c.OBS.AccessKey)
		decryptEngineCredential(c.undecryptable, "obs.secret_key", "storage obs secret_key", &c.OBS.SecretKey)
	}
}

// restoreStorageEngineCredentials puts preserved ciphertext back on a copy
// about to be encrypted for storage; EncryptAESGCM passes enc:v1: values
// through unchanged.
func restoreStorageEngineCredentials(c *StorageEngineConfig) {
	p := c.undecryptable
	if p == nil {
		return
	}
	if c.MinIO != nil {
		p.restore("minio.access_key_id", &c.MinIO.AccessKeyID)
		p.restore("minio.secret_access_key", &c.MinIO.SecretAccessKey)
	}
	if c.COS != nil {
		p.restore("cos.secret_id", &c.COS.SecretID)
		p.restore("cos.secret_key", &c.COS.SecretKey)
	}
	if c.TOS != nil {
		p.restore("tos.access_key", &c.TOS.AccessKey)
		p.restore("tos.secret_key", &c.TOS.SecretKey)
	}
	if c.S3 != nil {
		p.restore("s3.access_key", &c.S3.AccessKey)
		p.restore("s3.secret_key", &c.S3.SecretKey)
	}
	if c.OSS != nil {
		p.restore("oss.access_key", &c.OSS.AccessKey)
		p.restore("oss.secret_key", &c.OSS.SecretKey)
	}
	if c.KS3 != nil {
		p.restore("ks3.access_key", &c.KS3.AccessKey)
		p.restore("ks3.secret_key", &c.KS3.SecretKey)
	}
	if c.OBS != nil {
		p.restore("obs.access_key", &c.OBS.AccessKey)
		p.restore("obs.secret_key", &c.OBS.SecretKey)
	}
}

// encryptParserEngineCredentials encrypts parser credentials on a copy.
func encryptParserEngineCredentials(c *ParserEngineConfig) error {
	if err := encryptEngineCredential("parser mineru_api_key", &c.MinerUAPIKey); err != nil {
		return err
	}
	if err := encryptEngineCredential("parser mineru_server_api_key", &c.MinerUServerAPIKey); err != nil {
		return err
	}
	return encryptEngineCredential("parser paddleocr_vl_cloud_token", &c.PaddleOCRVLCloudToken)
}

// decryptParserEngineCredentials reverses the above in place.
func decryptParserEngineCredentials(c *ParserEngineConfig) {
	c.undecryptable = newPreservedCiphertext()
	decryptEngineCredential(c.undecryptable, "mineru_api_key", "parser mineru_api_key", &c.MinerUAPIKey)
	decryptEngineCredential(c.undecryptable, "mineru_server_api_key", "parser mineru_server_api_key", &c.MinerUServerAPIKey)
	decryptEngineCredential(c.undecryptable, "paddleocr_vl_cloud_token", "parser paddleocr_vl_cloud_token", &c.PaddleOCRVLCloudToken)
}

// restoreParserEngineCredentials puts preserved ciphertext back on a copy
// about to be encrypted for storage.
func restoreParserEngineCredentials(c *ParserEngineConfig) {
	p := c.undecryptable
	if p == nil {
		return
	}
	p.restore("mineru_api_key", &c.MinerUAPIKey)
	p.restore("mineru_server_api_key", &c.MinerUServerAPIKey)
	p.restore("paddleocr_vl_cloud_token", &c.PaddleOCRVLCloudToken)
}
