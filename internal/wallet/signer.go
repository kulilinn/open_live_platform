package wallet

import (
	"archive/zip"
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"go.mozilla.org/pkcs7"
)

var passAssets = [...]string{"icon.png", "icon@2x.png", "icon@3x.png", "logo.png", "logo@2x.png"}
var certificateUserID = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}

type signer struct {
	config     Config
	leaf, wwdr *x509.Certificate
	key        crypto.Signer
	assets     map[string][]byte
	loginURL   *url.URL
}

func openSigner(config Config, now time.Time) (*signer, error) {
	leaf, err := readCertificate(config.Certificate)
	if err != nil {
		return nil, fmt.Errorf("read Wallet signing certificate: %w", err)
	}
	wwdr, err := readCertificate(config.WWDRCertificate)
	if err != nil {
		return nil, fmt.Errorf("read Wallet WWDR certificate: %w", err)
	}
	for _, cert := range []*x509.Certificate{leaf, wwdr} {
		if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
			return nil, errors.New("Wallet certificate is not currently valid")
		}
	}
	if err := leaf.CheckSignatureFrom(wwdr); err != nil {
		return nil, errors.New("Wallet signing certificate was not issued by the configured WWDR certificate")
	}
	userID := false
	for _, name := range leaf.Subject.Names {
		if name.Type.Equal(certificateUserID) && name.Value == config.PassType {
			userID = true
			break
		}
	}
	if !userID || !slices.Contains(leaf.Subject.OrganizationalUnit, config.TeamID) {
		return nil, errors.New("Wallet pass type or team does not match the signing certificate")
	}
	key, err := readPrivateKey(config.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("read Wallet signing key: %w", err)
	}
	pk, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil || !bytes.Equal(pk, leaf.RawSubjectPublicKeyInfo) {
		return nil, errors.New("Wallet signing certificate and private key differ")
	}
	loginURL, err := walletURL(config.LoginURL, true)
	if err != nil {
		return nil, fmt.Errorf("Wallet login URL: %w", err)
	}
	if config.WebServiceURL != "" {
		if _, err := walletURL(config.WebServiceURL, false); err != nil {
			return nil, fmt.Errorf("Wallet web service URL: %w", err)
		}
	}
	assets := make(map[string][]byte, len(passAssets))
	for _, name := range passAssets {
		image, err := readAsset(filepath.Join(config.Assets, name))
		if err != nil {
			return nil, fmt.Errorf("read Wallet asset %s: %w", name, err)
		}
		assets[name] = image
	}
	return &signer{config: config, leaf: leaf, wwdr: wwdr, key: key, assets: assets, loginURL: loginURL}, nil
}

func readCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if block, _ := pem.Decode(data); block != nil {
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("expected a PEM certificate")
		}
		data = block.Bytes
	}
	return x509.ParseCertificate(data)
}

func readPrivateKey(path string) (crypto.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("expected a PEM private key")
	}
	var parsed any
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	default:
		return nil, errors.New("expected an unencrypted PEM private key")
	}
	if err != nil {
		return nil, err
	}
	switch key := parsed.(type) {
	case *rsa.PrivateKey:
		return key, nil
	case *ecdsa.PrivateKey:
		return key, nil
	default:
		return nil, errors.New("unsupported Wallet signing key")
	}
}

func readAsset(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxAsset = 8 << 20
	body, err := io.ReadAll(io.LimitReader(f, maxAsset+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAsset {
		return nil, errors.New("PNG file is too large")
	}
	if _, err := png.DecodeConfig(bytes.NewReader(body)); err != nil {
		return nil, errors.New("PNG file is invalid")
	}
	return body, nil
}

func walletURL(raw string, allowQuery bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (!allowQuery && u.RawQuery != "") {
		return nil, errors.New("expected an HTTPS URL")
	}
	return u, nil
}

func (s *signer) bundle(userID int64, token string, expires time.Time) ([]byte, error) {
	pass, err := s.passJSON(userID, token, expires)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"pass.json": pass}
	for name, data := range s.assets {
		files[name] = data
	}
	manifest := make(map[string]string, len(files))
	names := make([]string, 0, len(files)+2)
	for name, data := range files {
		hash := sha1.Sum(data) // The Wallet manifest requires SHA-1 hashes.
		manifest[name] = hex.EncodeToString(hash[:])
		names = append(names, name)
	}
	files["manifest.json"], err = json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	files["signature"], err = s.detachedSignature(files["manifest.json"])
	if err != nil {
		return nil, err
	}
	names = append(names, "manifest.json", "signature")
	slices.Sort(names)
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, name := range names {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return nil, err
		}
		if _, err = entry.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *signer) passJSON(userID int64, token string, expires time.Time) ([]byte, error) {
	login := *s.loginURL
	params := login.Query()
	params.Set("token", token)
	login.RawQuery = params.Encode()
	user := strconv.FormatInt(userID, 10)
	barcode := map[string]string{"format": "PKBarcodeFormatQR", "message": login.String(), "messageEncoding": "iso-8859-1"}
	data := map[string]any{
		"formatVersion": 1, "passTypeIdentifier": s.config.PassType, "serialNumber": user, "teamIdentifier": s.config.TeamID,
		"organizationName": "LoveRose", "description": "True love beyond the time and space", "logoText": "LoveRose",
		"foregroundColor": "rgb(255, 255, 255)", "backgroundColor": "rgb(200, 64, 126)", "labelColor": "rgb(255, 255, 255)",
		"barcode": barcode, "barcodes": []map[string]string{barcode}, "expirationDate": expires.UTC().Format(time.RFC3339),
		"storeCard": map[string][]map[string]string{
			"primaryFields": {{"key": "user_id", "label": "MEMBER ID", "value": user}},
			"backFields":    {{"key": "website", "label": "VISIT OUR SITE", "value": "TOUCH ME 🌹", "attributedValue": login.String()}},
		},
	}
	if s.config.WebServiceURL != "" {
		data["webServiceURL"] = s.config.WebServiceURL
		data["authenticationToken"] = token
	}
	return json.Marshal(data)
}

func (s *signer) detachedSignature(manifest []byte) ([]byte, error) {
	signed, err := pkcs7.NewSignedData(manifest)
	if err != nil {
		return nil, fmt.Errorf("sign Wallet manifest: %w", err)
	}
	signed.SetDigestAlgorithm(pkcs7.OIDDigestAlgorithmSHA256)
	if err := signed.AddSignerChain(s.leaf, s.key, []*x509.Certificate{s.wwdr}, pkcs7.SignerInfoConfig{}); err != nil {
		return nil, fmt.Errorf("sign Wallet manifest: %w", err)
	}
	signed.Detach()
	return signed.Finish()
}
