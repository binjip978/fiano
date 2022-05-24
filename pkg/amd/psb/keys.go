package psb

// Key parsing logic is based on AMD Platform Security Processor BIOS Architecture Design Guide for AMD Family 17h and Family 19h Processors
// Publication # 55758
// Issue Date: August 2020
// Revision: 1.11

import (
	"bytes"
	"crypto/rsa"
	"encoding/binary"
	"fmt"
	"math/big"

	"strings"

	amd_manifest "github.com/linuxboot/fiano/pkg/amd/manifest"
)

// KeyID is the primary identifier of a key
type KeyID buf16B

// Hex returns a hexadecimal string representation of a KeyID
func (kid *KeyID) Hex() string {
	var s strings.Builder
	fmt.Fprintf(&s, "%x", *kid)
	return s.String()
}

// String returns the hexadecimal string representation of a KeyID
func (kid *KeyID) String() string {
	return kid.Hex()
}

// KeyIDs represents a list of KeyID
type KeyIDs []KeyID

// String returns a string representation of all KeyIDs
func (kids KeyIDs) String() string {
	if len(kids) == 0 {
		return ""
	}

	var s strings.Builder
	fmt.Fprintf(&s, "%s", kids[0].Hex())
	for _, kid := range kids[1:] {
		fmt.Fprintf(&s, ", %s", kid.Hex())
	}
	return s.String()
}

// Key structure extracted from the firmware
type Key struct {
	VersionID       uint32
	KeyID           KeyID
	CertifyingKeyID buf16B
	KeyUsageFlag    uint32
	Reserved        buf16B
	ExponentSize    uint32
	ModulusSize     uint32
	Exponent        []byte
	Modulus         []byte
}

// Key creation functions manage two slightly different key structures available in firmware:
//
// 1) Those serialized into key tokens
// 2) Those serialized into the key database
//
// Format 2) is as follow:
//
// type key struct {
// 		dataSize uint32
//		version uint32
//		keyUsageFlag uint32
// 		publicExponent [4]uint8
//		keyID	[16]uint8
//		keySize uint32
//		reserved buf44B
//		modulus []byte
// }
//
// From a bytes buffer, there is no way to distinguish between the two cases above, so an indication
// of which format to use should come from the caller (by calling NewKey<TYPE>).
//
// Both formats will be deserialized into Key structure. Some fields of Key might contain zero value
// (e.g. certifying key ID for keys extracted from the key database, which is indirectly the AMD root
// key as it signs the whole key database).
//
// If the key is a token key, the signature is validated.
// Additional safety checks are implemented during serialization:
// if a `certifyingKeyID` is retrieved from the buffer and it's not null, a KeySet must be available for validation,
// or an error will be returned. Callers however are ultimately responsible to make sure that a KeySet is passed if
// a key should be validated.

func zeroCertifyingKeyID(key *Key) bool {
	for _, v := range key.CertifyingKeyID {
		if v != 0 {
			return false
		}
	}
	return true
}

// readExponent reads exponent value from a buffer, assuming exponent size
// has already been populated.
func readExponent(buff *bytes.Buffer, key *Key) error {
	if key.ExponentSize%8 != 0 {
		return newErrInvalidFormat(fmt.Errorf("exponent size is not divisible by 8"))
	}
	exponent := make([]byte, key.ExponentSize/8)
	if err := binary.Read(buff, binary.LittleEndian, &exponent); err != nil {
		return newErrInvalidFormat(fmt.Errorf("could not parse exponent: %w", err))
	}
	key.Exponent = exponent
	return nil
}

// readModulus reads modulus value from a buffer, assuming modulus size
// has already been populated
func readModulus(buff *bytes.Buffer, key *Key) error {
	if key.ModulusSize%8 != 0 {
		return newErrInvalidFormat(fmt.Errorf("modulus size is not divisible by 8"))
	}

	modulus := make([]byte, key.ModulusSize/8)
	if err := binary.Read(buff, binary.LittleEndian, &modulus); err != nil {
		return newErrInvalidFormat(fmt.Errorf("could not parse modulus: %w", err))
	}
	key.Modulus = modulus
	return nil
}

// newTokenOrRootKey creates the common parts of Token and Root keys
func newTokenOrRootKey(buff *bytes.Buffer) (*Key, error) {

	key := Key{}

	if err := binary.Read(buff, binary.LittleEndian, &key.VersionID); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse VersionID: %w", err))
	}
	if err := binary.Read(buff, binary.LittleEndian, &key.KeyID); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse KeyID: %w", err))
	}
	if err := binary.Read(buff, binary.LittleEndian, &key.CertifyingKeyID); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse Certifying KeyID: %w", err))
	}
	if err := binary.Read(buff, binary.LittleEndian, &key.KeyUsageFlag); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse Key Usage Flag: %w", err))
	}
	if err := binary.Read(buff, binary.LittleEndian, &key.Reserved); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse reserved area: %w", err))
	}
	if err := binary.Read(buff, binary.LittleEndian, &key.ExponentSize); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse exponent size: %w", err))
	}
	if err := binary.Read(buff, binary.LittleEndian, &key.ModulusSize); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse modulus size: %w", err))
	}

	if err := readExponent(buff, &key); err != nil {
		return nil, err
	}

	if err := readModulus(buff, &key); err != nil {
		return nil, err
	}
	return &key, nil
}

// NewRootKey creates a new root key object which is considered trusted without any need for signature check
func NewRootKey(buff *bytes.Buffer) (*Key, error) {
	key, err := newTokenOrRootKey(buff)
	if err != nil {
		return nil, fmt.Errorf("cannot parse root key: %w", err)
	}

	if key.KeyID != key.CertifyingKeyID {
		return nil, newErrInvalidFormat(fmt.Errorf("root key must have certifying key ID == key ID (key ID: %x, certifying key ID: %x)", key.KeyID, key.CertifyingKeyID))
	}
	return key, err

}

// NewTokenKey create a new key object from a signed token
func NewTokenKey(buff *bytes.Buffer, keySet KeySet) (*Key, error) {

	raw := buff.Bytes()

	key, err := newTokenOrRootKey(buff)
	if err != nil {
		return nil, fmt.Errorf("could not create new token key: %w", err)
	}

	// validate the signature of the new token key
	signature := make([]byte, key.ModulusSize/8)
	if err := binary.Read(buff, binary.LittleEndian, &signature); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse signature from key token: %w", err))
	}
	signingKeyID := KeyID(key.CertifyingKeyID)
	signingKey := keySet.GetKey(signingKeyID)
	if signingKey == nil {
		return nil, fmt.Errorf("could not find signing key with ID %s for key token key", signingKeyID.Hex())
	}

	// A key extracted from a signed token has the following structure:
	// * 64 bytes header
	// * exponent
	// * modulus
	// * signature.
	//
	// Exponent, modulus and signature are all of the same size. Only the latter is not signed, hence the lenght
	// of the signed payload is header size + 2 * exponent/modulus size.
	lenSigned := uint64(64 + 2*key.ModulusSize/8)
	if uint64(len(raw)) < lenSigned {
		return nil, newErrInvalidFormat(fmt.Errorf("length of signed token is not sufficient: expected > %d, got %d", lenSigned, len(raw)))
	}

	// Validate the signature of the raw token
	if _, err := NewSignedBlob(reverse(signature), raw[:lenSigned], signingKey, "token key"); err != nil {
		return nil, fmt.Errorf("could not validate the signature of token key: %w", err)
	}
	return key, nil
}

// NewKeyFromDatabase creates a new key object from key database entry
func NewKeyFromDatabase(buff *bytes.Buffer) (*Key, error) {
	key := Key{}

	var (
		dataSize uint32
		numRead  uint64
	)

	if err := readAndCountSize(buff, binary.LittleEndian, &dataSize, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse dataSize: %w", err))
	}

	// consider if we still have enough data to parse a whole key entry which is dataSize long.
	// dataSize includes the uint32 dataSize field itself
	if uint64(dataSize) > uint64(buff.Len())+4 {
		return nil, newErrInvalidFormat(fmt.Errorf("buffer is not long enough (%d) to satisfy dataSize (%d)", buff.Len(), dataSize))
	}

	if err := readAndCountSize(buff, binary.LittleEndian, &key.VersionID, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse VersionID: %w", err))
	}

	if err := readAndCountSize(buff, binary.LittleEndian, &key.KeyUsageFlag, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse key usage flags: %w", err))
	}

	var publicExponent buf4B
	if err := readAndCountSize(buff, binary.LittleEndian, &publicExponent, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse public exponent: %w", err))
	}
	key.Exponent = publicExponent[:]

	if err := readAndCountSize(buff, binary.LittleEndian, &key.KeyID, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse key id: %w", err))
	}

	var keySize uint32
	if err := readAndCountSize(buff, binary.LittleEndian, &keySize, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse key size: %w", err))
	}
	if keySize == 0 {
		return nil, newErrInvalidFormat(fmt.Errorf("key size cannot be 0"))
	}

	if keySize%8 != 0 {
		return nil, newErrInvalidFormat(fmt.Errorf("key size is not divisible by 8 (%d)", keySize))
	}

	key.ExponentSize = keySize
	key.ModulusSize = keySize

	var reserved buf44B
	if err := readAndCountSize(buff, binary.LittleEndian, &reserved, &numRead); err != nil {
		return nil, newErrInvalidFormat(fmt.Errorf("could not parse reserved area: %w", err))
	}
	// check if we have enough data left, based on keySize and dataSize
	if (numRead + uint64(keySize)/8) > uint64(dataSize) {
		return nil, newErrInvalidFormat(fmt.Errorf("inconsistent header, read so far %d, total size is %d, key size to read is %d, which goes out of bound", numRead, dataSize, keySize))
	}

	if err := readModulus(buff, &key); err != nil {
		return nil, err
	}

	if !zeroCertifyingKeyID(&key) {
		return nil, newErrInvalidFormat(fmt.Errorf("key extracted from key database should have zero certifying key ID"))
	}

	return &key, nil
}

// String returns a string representation of the key
func (k *Key) String() string {
	var s strings.Builder

	pubKey, err := k.Get()
	if err != nil {
		fmt.Fprintf(&s, "could not get RSA key from raw bytes: %v\n", err)
		return s.String()
	}

	fmt.Fprintf(&s, "Version ID: 0x%x\n", k.VersionID)
	fmt.Fprintf(&s, "Key ID: 0x%s\n", k.KeyID.Hex())
	fmt.Fprintf(&s, "Certifying Key ID: 0x%x\n", k.CertifyingKeyID)
	fmt.Fprintf(&s, "Key Usage Flag: 0x%x\n", k.KeyUsageFlag)
	fmt.Fprintf(&s, "Exponent size: 0x%x (dec %d) \n", k.ExponentSize, k.ExponentSize)
	fmt.Fprintf(&s, "Modulus size: 0x%x (dec %d)\n", k.ModulusSize, k.ModulusSize)

	switch rsaKey := pubKey.(type) {
	case *rsa.PublicKey:
		fmt.Fprintf(&s, "Exponent: 0x%d\n", rsaKey.E)
	default:
		fmt.Fprintf(&s, "Exponent: key is not RSA, cannot get decimal exponent\n")
	}

	fmt.Fprintf(&s, "Modulus: 0x%x\n", k.Modulus)
	return s.String()
}

// Get returns the PublicKey object from golang standard library.
// AMD Milan supports only RSA Keys (2048, 4096), future platforms
// might add support for additional key types.
func (k *Key) Get() (interface{}, error) {

	if len(k.Exponent) == 0 {
		return nil, fmt.Errorf("could not build public key without exponent")
	}
	if len(k.Modulus) == 0 {
		return nil, fmt.Errorf("could not build public key without modulus")
	}

	N := big.NewInt(0)
	E := big.NewInt(0)

	// modulus and exponent are read as little endian
	rsaPk := rsa.PublicKey{N: N.SetBytes(reverse(k.Modulus)), E: int(E.SetBytes(reverse(k.Exponent)).Int64())}
	return &rsaPk, nil
}

// GetKeys returns all the keys known to the system in the form of a KeySet.
// The firmware itself contains a key database, but that is not comprehensive
// of all the keys known to the system (e.g. additional keys might be OEM key,
// ABL signing key, etc.).
func GetKeys(amdFw *amd_manifest.AMDFirmware, level uint) (KeySet, error) {

	keySet := NewKeySet()
	err := getKeysFromDatabase(amdFw, level, keySet)
	if err != nil {
		return keySet, fmt.Errorf("could not get key from table into KeySet: %w", err)
	}

	// Extract ABL signing key (entry 0x0A in PSP Directory), which is signed with AMD Public Key.
	pubKeyBytes, err := ExtractPSPEntry(amdFw, level, ABLPublicKey)
	if err != nil {
		return keySet, fmt.Errorf("could not extract raw PSP entry for ABL Public Key: %w", err)
	}
	ablPk, err := NewTokenKey(bytes.NewBuffer(pubKeyBytes), keySet)
	if err != nil {
		return keySet, fmt.Errorf("could not extract ABL public key: %w", err)
	}

	err = keySet.AddKey(ablPk, ABLKey)
	if err != nil {
		return keySet, fmt.Errorf("could not add ABL signing key to key set: %w", err)
	}

	// Extract OEM signing key (entry 0x05 in BIOS Directory table)
	pubKeyBytes, err = ExtractBIOSEntry(amdFw, level, OEMSigningKeyEntry, 0)
	if err != nil {
		return keySet, fmt.Errorf("could not extract raw BIOS directory entry for OEM Public Key: %w", err)
	}
	oemPk, err := NewTokenKey(bytes.NewBuffer(pubKeyBytes), keySet)
	if err != nil {
		return keySet, fmt.Errorf("could not extract OEM public key: %w", err)
	}

	err = keySet.AddKey(oemPk, OEMKey)
	if err != nil {
		return keySet, fmt.Errorf("could not add OEM signing key to key set: %w", err)
	}

	return keySet, err
}
