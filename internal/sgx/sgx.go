package sgx

/*
#cgo CFLAGS: -g -Wall -I /usr/local/include
#cgo LDFLAGS: -lp11sgx -L /usr/local/lib

#include <cryptoki.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <sgx_pce.h>
#include <QuoteGeneration.h>

CK_ULONG quote_offset(CK_BYTE_PTR bytes) {
	CK_RSA_PUBLIC_KEY_PARAMS* params = (CK_RSA_PUBLIC_KEY_PARAMS*)bytes;
	if (params == NULL) {
		return 0;
	}
	CK_ULONG pubKeySize = params->ulModulusLen + params->ulExponentLen;
	// check for overflow
	if (pubKeySize < params->ulModulusLen || pubKeySize < params->ulExponentLen) {
		return 0;
	}
    CK_ULONG offset = sizeof(CK_RSA_PUBLIC_KEY_PARAMS) + pubKeySize;

	return offset;
}

CK_ULONG params_size(CK_BYTE_PTR bytes) {
    CK_ULONG offset = sizeof(CK_RSA_PUBLIC_KEY_PARAMS);
	return offset;
}

CK_ULONG ulModulusLen_offset(CK_BYTE_PTR bytes) {
	CK_RSA_PUBLIC_KEY_PARAMS* params = (CK_RSA_PUBLIC_KEY_PARAMS*)bytes;
	if (params == NULL) {
		return 0;
	}
	CK_ULONG offset = params->ulModulusLen;
	return offset;
}

CK_ULONG ulExponentLen_offset(CK_BYTE_PTR bytes) {
	CK_RSA_PUBLIC_KEY_PARAMS* params = (CK_RSA_PUBLIC_KEY_PARAMS*)bytes;
	if (params == NULL) {
		return 0;
	}
	CK_ULONG offset = params->ulExponentLen;
	return offset;
}

*/
import "C"

import (
	"crypto"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"math/big"
	"os/exec"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ThalesIgnite/crypto11"
	"github.com/go-logr/logr"
	"istio.io/pkg/env"
	"istio.io/pkg/log"

	"github.com/miekg/pkcs11"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	SgxLibrary                 = "/home/istio-proxy/sgx/lib/libp11sgx.so"
	DefaultTokenLabel          = "HSMSDSServer"
	HSMKeyLabel                = "default"
	DefaultHSMSoPin            = "HSMSoPin"
	DefaultHSMUserPin          = "HSMUserPin"
	DefaultHSMKeyType          = "rsa"
	DefaultRSAKeySize          = 3072
	EnclaveQuoteKeyObjectLabel = "Enclave Quote"
)

const (
	// MinRSAKeySize is the minimum RSA keysize allowed to be generated by the
	// generator functions in this package.
	MinRSAKeySize = 2048

	// MaxRSAKeySize is the maximum RSA keysize allowed to be generated by the
	// generator functions in this package.
	MaxRSAKeySize = 8192

	// ECCurve256 represents a secp256r1 / prime256v1 / NIST P-256 ECDSA key.
	ECCurve256 = 256
	// ECCurve384 represents a secp384r1 / NIST P-384 ECDSA key.
	ECCurve384 = 384
	// ECCurve521 represents a secp521r1 / NIST P-521 ECDSA key.
	ECCurve521 = 521
)

var (
	HSMTokenLabel = env.RegisterStringVar("TokenLabel", DefaultTokenLabel, "PKCS11 label to use for the token.").Get()
	HSMUserPin    = env.RegisterStringVar("UserPin", DefaultHSMUserPin, "PKCS11 token user pin.").Get()
	HSMSoPin      = env.RegisterStringVar("Sopin", DefaultHSMSoPin, "PKCS11 token so/admin pin.").Get()
	HSMKeyType    = env.RegisterStringVar("KeyType", DefaultHSMKeyType, "PKCS11 key type.").Get()
)

type SgxContext struct {
	// pkcs11 is needed for quote generation.
	// There is no way to wrap/unwrap key using crypto11
	p11Ctx *pkcs11.Ctx
	// session opened for quote generation
	p11Session pkcs11.SessionHandle
	// private key used for quote generation
	quotePrvKey pkcs11.ObjectHandle
	// private key used for quote generation
	quotePubKey pkcs11.ObjectHandle
	// generated quote
	ctkQuote []byte

	cryptoCtx     *crypto11.Context
	cryptoCtxLock sync.Mutex
	cfg           *Config

	// self signed signers
	pendingSelfSignedSigners map[string]struct{}
	selfSignedSignerNames    []string
	// k8sClient client.Client
	qaCounter uint64
	log       logr.Logger
}

type Config struct {
	HSMTokenLabel string
	HSMUserPin    string
	HSMSoPin      string
	HSMKeyLabel   string
	HSMKeyType    string
	HSMConfigPath string
}

func (cfg *Config) Validate() error {
	if len(cfg.HSMTokenLabel) == 0 {
		cfg.HSMTokenLabel = DefaultTokenLabel
		log.Warnf("Missing HSM Token Label")
	}

	if len(cfg.HSMSoPin) == 0 {
		cfg.HSMSoPin = DefaultHSMSoPin
		log.Warnf("Missing HSM So pin")
	}

	if len(cfg.HSMUserPin) == 0 {
		log.Warnf("Missing HSM User pin")
		cfg.HSMUserPin = DefaultHSMUserPin
	}

	if len(cfg.HSMKeyType) == 0 {
		log.Warnf("Missing HSM Key Type")
		cfg.HSMKeyType = DefaultHSMKeyType
	}

	return nil
}

func NewContext(cfg Config) (*SgxContext, error) {
	ctx := &SgxContext{
		cfg:                      &cfg,
		log:                      ctrl.Log.WithName("SGX"),
		pendingSelfSignedSigners: map[string]struct{}{},
	}
	if err := ctx.reloadCryptoContext(); err != nil {
		if err.Error() == "could not find PKCS#11 token" /* crypto11.errNotFoundError */ {
			ctx.log.V(3).Info("No existing token found, creating new token...")
			if err := ctx.initializeToken(); err != nil {
				return nil, err
			}
		} else {
			ctx.log.V(2).Info("Failed to configure command")
			return nil, err
		}
	}

	// provision CA key using QuoteAttestation CRD
	ctx.p11Ctx = pkcs11.New(SgxLibrary)

	ctx.log.Info("Initiating p11Session...")
	sh, err := initP11Session(ctx.p11Ctx, cfg.HSMTokenLabel)
	if err != nil {
		ctx.Destroy()
		return nil, err
	}
	ctx.p11Session = sh

	return ctx, nil
}

func (ctx *SgxContext) Destroy() {
	ctx.destroyP11Context()
	ctx.destroyCryptoContext()
}

func (ctx *SgxContext) Quote() ([]byte, error) {
	if ctx.ctkQuote == nil {
		return nil, fmt.Errorf("empty quote")
	}
	strQuote := base64.StdEncoding.EncodeToString(ctx.ctkQuote)
	return []byte(strQuote), nil
}

// QuotePublicKey returns the base64 encoded key
// used for quote generation
func (ctx *SgxContext) QuotePublicKey() ([]byte, error) {
	// ctx.p11CtxLock.Lock()
	// defer ctx.p11CtxLock.Unlock()

	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_MODULUS, nil),
		pkcs11.NewAttribute(pkcs11.CKA_PUBLIC_EXPONENT, nil),
	}
	attrs, err := ctx.p11Ctx.GetAttributeValue(ctx.p11Session, ctx.quotePubKey, template)
	if err != nil {
		return nil, err
	}
	var modulus = new(big.Int)
	modulus.SetBytes(attrs[0].Value)
	var bigExponent = new(big.Int)
	bigExponent.SetBytes(attrs[1].Value)
	if bigExponent.BitLen() > 32 || bigExponent.Sign() < 1 {
		return nil, fmt.Errorf("malformed quote public key")
	}
	exponent := int(bigExponent.Uint64())
	key := rsa.PublicKey{
		N: modulus,
		E: exponent,
	}

	return EncodePublicKey(&key)
}

func (ctx *SgxContext) TokenLabel() (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("invalid SGX context")
	}
	return ctx.cfg.HSMTokenLabel, nil
}

func (ctx *SgxContext) GetCryptoContext() (*crypto11.Context, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid SGX context")
	}
	return ctx.cryptoCtx, nil
}

func (ctx *SgxContext) GetConfig() (*Config, error) {
	if ctx == nil {
		return nil, fmt.Errorf("invalid SGX context")
	}
	return ctx.cfg, nil
}

func (ctx *SgxContext) destroyP11Context() {
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()
	if ctx.p11Ctx != nil {
		ctx.p11Ctx.Logout(ctx.p11Session)
		ctx.p11Ctx.DestroyObject(ctx.p11Session, ctx.quotePrvKey)
		ctx.p11Ctx.DestroyObject(ctx.p11Session, ctx.quotePubKey)
		ctx.p11Ctx.CloseSession(ctx.p11Session)
		ctx.p11Ctx.Destroy()
		ctx.p11Ctx = nil
	}
}

func (ctx *SgxContext) destroyCryptoContext() {
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()
	if ctx.cryptoCtx != nil {
		ctx.cryptoCtx.Close()
		ctx.cryptoCtx = nil
	}
}

func (ctx *SgxContext) reloadCryptoContext() error {
	ctx.destroyCryptoContext()

	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()

	cryptoCtx, err := crypto11.Configure(&crypto11.Config{
		Path:       SgxLibrary,
		TokenLabel: ctx.cfg.HSMTokenLabel,
		Pin:        ctx.cfg.HSMUserPin,
	})
	if err != nil {
		return err
	}
	ctx.cryptoCtx = cryptoCtx
	return nil
}

func (ctx *SgxContext) initializeToken() error {
	cmd := exec.Command("pkcs11-tool", "--module", SgxLibrary, "--init-token",
		"--init-pin", "--slot-index", fmt.Sprintf("%d", 0), "--label", ctx.cfg.HSMTokenLabel,
		"--pin", ctx.cfg.HSMUserPin, "--so-pin", ctx.cfg.HSMSoPin)

	if err := cmd.Run(); err != nil {
		// ctx.log.Info("command", cmd.Args, "output", cmd.Stdout)
		log.Info("command", cmd.Args, "output", cmd.Stdout)
		return fmt.Errorf("failed to initialize token: %v", err)
	}

	return ctx.reloadCryptoContext()
}

func initP11Session(p11Ctx *pkcs11.Ctx, tokenLabel string) (pkcs11.SessionHandle, error) {
	slot, err := findP11Slot(p11Ctx, tokenLabel)
	if err != nil {
		return 0, err
	}

	p11Session, err := p11Ctx.OpenSession(slot, pkcs11.CKF_SERIAL_SESSION|pkcs11.CKF_RW_SESSION)
	if err != nil {
		return 0, fmt.Errorf("pkcs11: failed to open session: %v", err)
	}
	return p11Session, nil
}

func findP11Slot(p11Ctx *pkcs11.Ctx, tokenLabel string) (uint, error) {
	list, err := p11Ctx.GetSlotList(true)
	if err != nil {
		return 0, fmt.Errorf("pkcs11: failed to get slot list: %v", err)
	}
	if len(list) == 0 {
		return 0, fmt.Errorf("pkcs11: no slots available")
	}

	for _, slot := range list {
		tInfo, err := p11Ctx.GetTokenInfo(slot)
		if err != nil {
			return 0, fmt.Errorf("pkcs11: failed to get token info(%d): %v", slot, err)
		}

		if tInfo.Label == tokenLabel {
			return slot, nil
		}
	}

	return 0, fmt.Errorf("pkcs11: token not found")
}

func generateP11KeyPair(p11Ctx *pkcs11.Ctx, p11Session pkcs11.SessionHandle) (pkcs11.ObjectHandle, pkcs11.ObjectHandle, error) {
	keyID, err := generateKeyID(rand.Reader, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to generate key-id: %v", err)
	}

	public := crypto11.AttributeSet{}
	public.AddIfNotPresent([]*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, false),
		pkcs11.NewAttribute(pkcs11.CKA_ENCRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_VERIFY, true),
		pkcs11.NewAttribute(pkcs11.CKA_WRAP, true),
		pkcs11.NewAttribute(pkcs11.CKA_MODULUS_BITS, DefaultRSAKeySize),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_RSA),
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PUBLIC_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, EnclaveQuoteKeyObjectLabel),
		pkcs11.NewAttribute(pkcs11.CKA_ID, keyID),
	})

	private := crypto11.AttributeSet{}
	private.AddIfNotPresent([]*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, false),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_SIGN, true),
		pkcs11.NewAttribute(pkcs11.CKA_UNWRAP, true),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_RSA),
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PRIVATE_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_ID, keyID),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, true),
	})

	// Generate a keypair used to generate and exchange SGX enclabe quote
	return p11Ctx.GenerateKeyPair(p11Session, []*pkcs11.Mechanism{
		pkcs11.NewMechanism(pkcs11.CKM_RSA_PKCS_KEY_PAIR_GEN, nil),
	}, public.ToSlice(), private.ToSlice())
}

func generateKeyID(reader io.Reader, len uint) ([]byte, error) {
	keyID := make([]byte, len)
	if _, err := reader.Read(keyID); err != nil {
		return nil, fmt.Errorf("failed to read random bytes: %v", err)
	}

	return keyID, nil
}

// newCACertificate returns a self-signed certificate used as certificate authority
func newCACertificate(key crypto.Signer) (*x509.Certificate, error) {
	max := new(big.Int).SetInt64(math.MaxInt64)
	serial, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		Version:               tls.VersionTLS12,
		SerialNumber:          serial,
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour * 24 * 365).UTC(),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		Subject: pkix.Name{
			CommonName:   "SGX self-signed root certificate authority",
			Organization: []string{"Intel(R) Corporation"},
		},
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	*tmpl = x509.Certificate{}
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		return nil, err
	}

	runtime.SetFinalizer(cert, func(c *x509.Certificate) {
		*c = x509.Certificate{}
	})

	return cert, nil
}

func (ctx *SgxContext) InitializeKey(keyLabel, keyAlgo string, keySize int) error {
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()

	reader, err := ctx.cryptoCtx.NewRandomReader()
	if err != nil {
		return fmt.Errorf("failed to initialize random reader: %v", err)
	}
	keyID, err := generateKeyID(reader, 32)
	if err != nil {
		return err
	}
	// crypto11 does not support the `Ed25519` key algorithm at this moment.
	switch keyAlgo {
	case "rsa":
		if keySize != 2048 && keySize != 4096 && keySize != 8192 {
			// We default the RSA key size to 2048.
			ctx.log.Info("Unspecified or invalid RSA key size, valid values are '2048', '4096' or '8192', defaulting to 2048")
			keySize = MinRSAKeySize
		}
		_, err = ctx.cryptoCtx.GenerateRSAKeyPairWithLabel(keyID, []byte(keyLabel), keySize)
	case "ecdsa":
		var ecCurve elliptic.Curve

		switch keySize {
		case ECCurve256:
			ecCurve = elliptic.P256()
		case ECCurve384:
			ecCurve = elliptic.P384()
		case ECCurve521:
			ecCurve = elliptic.P521()
		default:
			// We default the ECDSA curve to P256.
			ctx.log.Info("Unspecified or invalid ECDSA curve, valid values are '256', '384' or '521', defaulting to 256")
			ecCurve = elliptic.P256()
		}
		_, err = ctx.cryptoCtx.GenerateECDSAKeyPairWithLabel(keyID, []byte(keyLabel), ecCurve)
	default:
		// We default the unspecified/invalid key params to RSA 2048.
		ctx.log.Info("Unspecified or invalid key algorithm, defaulting to RSA 2048")
		_, err = ctx.cryptoCtx.GenerateRSAKeyPairWithLabel(keyID, []byte(keyLabel), MinRSAKeySize)
	}
	if err != nil {
		return err
	}

	ctx.log.Info("Crypto Keypair generated")
	log.Info("SGX: Crypto Keypair generated")
	return nil
}

func (ctx *SgxContext) RemoveKey(keyLabel string) error {
	if ctx == nil || ctx.cryptoCtx == nil {
		return fmt.Errorf("sgx context not initialized")
	}
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()
	privKey, err := ctx.cryptoCtx.FindKeyPair(nil, []byte(keyLabel))
	if err != nil {
		return fmt.Errorf("can't find pkcs11 keypair: %v", err)
	}
	if privKey != nil {
		dErr := privKey.Delete()
		if dErr != nil {
			return dErr
		}
	}
	return nil
}

func (ctx *SgxContext) GenerateQuoteAndPublicKey() error {
	pub, priv, err := generateP11KeyPair(ctx.p11Ctx, ctx.p11Session)
	if err != nil {
		ctx.Destroy()
		return fmt.Errorf("call to generateP11KeyPair failed %s", err)
	}
	ctx.quotePubKey = pub
	ctx.quotePrvKey = priv

	quote, err := generateQuote(ctx.p11Ctx, ctx.p11Session, pub)
	//quote := []byte("aaaa")
	if err != nil {
		ctx.p11Ctx.Destroy()
		return fmt.Errorf("call to generateQuote failed %s", err)
	}
	ctx.ctkQuote = quote
	return nil
}

func generateQuote(p11Ctx *pkcs11.Ctx, p11Session pkcs11.SessionHandle, pubKey pkcs11.ObjectHandle) ([]byte, error) {
	// Wrap the key
	quoteParams := C.CK_ECDSA_QUOTE_RSA_PUBLIC_KEY_PARAMS{
		qlPolicy: C.SGX_QL_PERSISTENT,
	}
	for i := 0; i < C.NONCE_LENGTH; i++ {
		quoteParams.nonce[i] = C.CK_BYTE(i)
	}

	params := C.GoBytes(unsafe.Pointer(&quoteParams), C.int(unsafe.Sizeof(quoteParams)))
	m := pkcs11.NewMechanism(C.CKM_EXPORT_ECDSA_QUOTE_RSA_PUBLIC_KEY, params)

	//	l.V(3).Info("Wrapping key....")
	quotePubKey, err := p11Ctx.WrapKey(p11Session, []*pkcs11.Mechanism{m}, pkcs11.ObjectHandle(0), pubKey)
	if err != nil {
		return nil, err
	}

	offset := int(C.quote_offset(*(*C.CK_BYTE_PTR)(unsafe.Pointer(&quotePubKey))))
	return quotePubKey[offset:], nil
}

func (ctx *SgxContext) GetSignerForName(name string) (crypto11.Signer, error) {
	if ctx == nil || ctx.cryptoCtx == nil {
		return nil, fmt.Errorf("sgx context not initialized")
	}
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()

	return ctx.cryptoCtx.FindKeyPair(nil, []byte(name))
}

// This method should be called on reply getting from key-manager
// after successful quote validation.
func (ctx *SgxContext) ProvisionKey(signerName string, base64Data []byte) error {
	decodedData, err := base64.StdEncoding.DecodeString(string(base64Data))
	if err != nil {
		return fmt.Errorf("corrupted key data: %v", err)
	}

	// Wrapped SWK - AES256 (with input public key) + Wrapped input private key (with SWK),
	// bytes concatenated and then encoded with base64 - After decoding with base64,
	// the first 384 bytes (3072 bits - it depends on the length of the input public key)
	// is SWK key (AES), the rest is a wrapped private key in PKCS#8 format
	wrappedSwk := decodedData[:DefaultRSAKeySize/8]
	wrappedPrKey := decodedData[DefaultRSAKeySize/8:]

	return ctx.provisionKey(signerName, wrappedSwk, wrappedPrKey)
}

func (ctx *SgxContext) provisionKey(keyLabel string, wrappedSWK []byte, wrappedKey []byte) error {
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()

	pCtx := ctx.p11Ctx
	attributeSWK := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_SECRET_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_AES),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, false),
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, false),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, false),
	}

	rsaPkcsOaepMech := pkcs11.NewMechanism(pkcs11.CKM_RSA_PKCS_OAEP, pkcs11.NewOAEPParams(pkcs11.CKM_SHA_1, pkcs11.CKG_MGF1_SHA1, pkcs11.CKZ_DATA_SPECIFIED, nil))
	swkHandle, err := pCtx.UnwrapKey(ctx.p11Session, []*pkcs11.Mechanism{rsaPkcsOaepMech}, ctx.quotePrvKey, wrappedSWK, attributeSWK)
	if err != nil {
		return fmt.Errorf("failed to unwrap symmetric wrapping key: %v", err)
	}

	ctx.log.Info("Unwrapped SWK Key successfully")

	keyID, err := generateKeyID(rand.Reader, 16)
	attributeWPK := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PRIVATE_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, pkcs11.CKK_RSA),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, keyLabel),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
		pkcs11.NewAttribute(pkcs11.CKA_PRIVATE, true),
		pkcs11.NewAttribute(pkcs11.CKA_DECRYPT, true),
		pkcs11.NewAttribute(pkcs11.CKA_SIGN, true),
		pkcs11.NewAttribute(pkcs11.CKA_UNWRAP, false),
		pkcs11.NewAttribute(pkcs11.CKA_SENSITIVE, true),
		pkcs11.NewAttribute(pkcs11.CKA_EXTRACTABLE, false),
		pkcs11.NewAttribute(pkcs11.CKA_ID, keyID),
	}
	aesKeyWrapMech := pkcs11.NewMechanism(pkcs11.CKM_AES_KEY_WRAP_PAD, nil)
	prvKey, err := pCtx.UnwrapKey(ctx.p11Session, []*pkcs11.Mechanism{aesKeyWrapMech}, swkHandle, wrappedKey, attributeWPK)
	if err != nil {
		return fmt.Errorf("failed to unwrap private key: %v", err)
	}
	ctx.log.Info("Unwrapped PWK Key successfully")

	template := []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_KEY_TYPE, nil),
		pkcs11.NewAttribute(pkcs11.CKA_MODULUS, nil),
		pkcs11.NewAttribute(pkcs11.CKA_PUBLIC_EXPONENT, nil),
	}
	publicKeyAttrs, err := ctx.p11Ctx.GetAttributeValue(ctx.p11Session, prvKey, template)
	if err != nil {
		ctx.log.Info("Failed to fetch public attributes: %v", err)
	}
	publicKeyAttrs = append(publicKeyAttrs, []*pkcs11.Attribute{
		pkcs11.NewAttribute(pkcs11.CKA_CLASS, pkcs11.CKO_PUBLIC_KEY),
		pkcs11.NewAttribute(pkcs11.CKA_LABEL, keyLabel),
		pkcs11.NewAttribute(pkcs11.CKA_TOKEN, true),
		pkcs11.NewAttribute(pkcs11.CKA_ID, keyID),
	}...)
	if _, err := ctx.p11Ctx.CreateObject(ctx.p11Session, publicKeyAttrs); err != nil {
		ctx.log.Info("Failed to add public key object", "error", err)
	}
	ctx.log.Info("Unwrapped Public Key successfully")

	return nil
}

func (ctx *SgxContext) RemoveKeyForSigner(name string) error {
	if ctx == nil || ctx.cryptoCtx == nil {
		return fmt.Errorf("sgx context not initialized")
	}
	ctx.cryptoCtxLock.Lock()
	defer ctx.cryptoCtxLock.Unlock()
	signer, err := ctx.cryptoCtx.FindKeyPair(nil, []byte(name))
	if err != nil {
		return err
	}
	if signer != nil {
		dErr := signer.Delete()
		if dErr != nil {
			return dErr
		}
	}
	return nil
}
