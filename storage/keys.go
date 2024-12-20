package storage

import (
	"fmt"
	"math/big"

	"github.com/vocdoni/vocdoni-z-sandbox/crypto/ecc"
	"github.com/vocdoni/vocdoni-z-sandbox/crypto/ecc/curves"
	"github.com/vocdoni/vocdoni-z-sandbox/types"
)

// SetEncryptionKeys stores the encryption keys for a process.
func (s *Storage) SetEncryptionKeys(pid types.ProcessID, publicKey ecc.Point, privateKey *big.Int) error {
	x, y := publicKey.Point()
	eks := EncryptionKeys{
		X:          x,
		Y:          y,
		PrivateKey: privateKey,
	}

	return s.setArtifact(encryptionKeyPrefix, pid.Marshal(), eks)
}

// EncryptionKeys loads the encryption keys for a process. Returns ErrNotFound if the keys do not exist
func (s *Storage) EncryptionKeys(pid types.ProcessID) (ecc.Point, *big.Int, error) {
	artifact, err := s.getArtifact(encryptionKeyPrefix, pid.Marshal())
	if err != nil {
		return nil, nil, fmt.Errorf("could not read encryption keys: %w", err)
	}
	if artifact == nil {
		return nil, nil, ErrNotFound
	}
	eks, ok := artifact.(EncryptionKeys)
	if !ok {
		panic("unexpected artifact type")
	}
	pubKey := curves.New(curves.CurveTypeBN254).SetPoint(eks.X, eks.Y)
	return pubKey, eks.PrivateKey, nil
}
