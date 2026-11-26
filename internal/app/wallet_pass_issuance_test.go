package app_test

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"image"
	"image/png"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"open_live_platform/internal/app"
	"open_live_platform/internal/auth"
	"open_live_platform/internal/store/postgres"
	"open_live_platform/internal/upload"
	"open_live_platform/internal/wallet"
)

func TestPostgresIssueAndDownloadAppleWalletPass(t *testing.T) {
	config, db := postgresTestDatabase(t)
	signing, root, openssl := walletTestCertificates(t)
	var locker sync.Mutex
	files := map[string][]byte{}
	putRequests := 0
	rejectPut := false
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		locker.Lock()
		defer locker.Unlock()
		if !strings.HasPrefix(r.URL.Path, "/wallet-fixture/tenant/passes/") ||
			!strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=wallet-key/") {
			t.Error("Wallet S3 request was not scoped and signed")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPut:
			putRequests++
			body, err := io.ReadAll(r.Body)
			digest := sha256.Sum256(body)
			if err != nil || r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(digest[:]) {
				t.Error("Wallet S3 upload content hash is incorrect")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			files[r.URL.Path] = body
			if rejectPut {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			body, found := files[r.URL.Path]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write(body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(objects.Close)
	config.WalletPasses = upload.PassConfig{Storage: upload.Config{Bucket: "wallet-fixture", Endpoint: objects.URL, Parent: "tenant",
		KeyID: "wallet-key", Secret: "test-wallet-storage-secret"}}
	config.JWTSecret = "wallet-test-only"
	platform, err := app.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	account := subscriptionTestUser(t, platform, "wallet-account", false)
	store, err := postgres.Open(t.Context(), config.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Shutdown)
	issue := func(email string) (string, error) {
		return wallet.Issue(t.Context(), signing, store, store, config.WalletPasses.Storage, config.JWTSecret, account.User.ID, email)
	}
	if _, err := issue("other@example.test"); err != wallet.ErrPassAccount {
		t.Fatalf("wrong email issuance error = %v", err)
	}
	claim, err := issue(account.User.Email)
	if err != nil {
		t.Fatal(err)
	}
	claimURL, err := url.Parse(claim)
	if err != nil || claimURL.Host != "wallet.example.test" || !strings.HasPrefix(claimURL.Path, "/public/pass/") {
		t.Fatalf("download link = %q: %v", claim, err)
	}
	ticket := path.Base(claimURL.Path)
	download := func(id string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/pass/"+id, nil)
		w := httptest.NewRecorder()
		platform.Server.Handler.ServeHTTP(w, r)
		return w
	}
	w := download(ticket)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/vnd.apple.pkpass" ||
		w.Header().Get("Content-Disposition") != "attachment; filename="+ticket+".pkpass" {
		t.Fatalf("download status=%d headers=%v body=%q", w.Code, w.Header(), w.Body.Bytes()[:min(160, w.Body.Len())])
	}
	claims := verifyWalletBundle(t, w.Body.Bytes(), signing, root, openssl, account.User.ID, config.JWTSecret)
	if strconv.FormatInt(claims.JTI, 10) != ticket || claims.Iss != account.User.Email || claims.Exp-claims.Iat != int64(signing.TokenTTL/time.Second) ||
		claims.Aud != "" || claims.Typ != auth.WalletPassType {
		t.Fatalf("legacy PASS claims fields do not match")
	}
	var duration, tokenDuration int64
	if err := db.QueryRow(t.Context(), `SELECT extract(epoch FROM (expires_at-published_at))::bigint,
		extract(epoch FROM (token_expires_at-issued_at))::bigint FROM wallet_passes WHERE id=$1`, claims.JTI).Scan(&duration, &tokenDuration); err != nil ||
		duration != int64(wallet.DownloadTTL/time.Second) || tokenDuration != int64(signing.TokenTTL/time.Second) {
		t.Fatalf("download and token durations = %d/%d, err=%v", duration, tokenDuration, err)
	}
	if _, err := db.Exec(t.Context(), `UPDATE wallet_passes SET issued_at=now()-interval '48 hour',expires_at=now()-interval '24 hour'
		WHERE id=$1`, claims.JTI); err != nil {
		t.Fatal(err)
	}
	if res := download(ticket); !strings.Contains(res.Body.String(), `"status":"4005"`) {
		t.Fatal("expired download remained available")
	}
	locker.Lock()
	rejectPut = true
	callsBeforeFailure := putRequests
	locker.Unlock()
	if callsBeforeFailure != 1 {
		t.Fatalf("unexpected uploads before failure: %d", callsBeforeFailure)
	}
	if out, err := issue(account.User.Email); err == nil || out != "" {
		t.Fatal("failed upload generated a usable ticket")
	}
	var pending int64
	if err := db.QueryRow(t.Context(), `SELECT id FROM wallet_passes WHERE published_at IS NULL`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if res := download(strconv.FormatInt(pending, 10)); !strings.Contains(res.Body.String(), `"status":"4005"`) {
		t.Fatal("an object with unconfirmed upload was available")
	}
}

func walletTestCertificates(t *testing.T) (wallet.Config, string, string) {
	t.Helper()
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		openssl = filepath.Join(os.Getenv("ProgramFiles"), "Git", "usr", "bin", "openssl.exe")
		if _, err := os.Stat(openssl); err != nil {
			t.Skip("OpenSSL is required to independently verify the test Wallet signature")
		}
	}
	dir := t.TempDir()
	cfg := wallet.Config{Assets: dir, Certificate: filepath.Join(dir, "pass.pem"), PrivateKey: filepath.Join(dir, "private.pem"),
		WWDRCertificate: filepath.Join(dir, "wwdr.cer"), PassType: "pass.test.rose", TeamID: "GOODTEAM1",
		PublicURL: "https://wallet.example.test/public", LoginURL: "https://members.example.test/?lang=en",
		WebServiceURL: "https://updates.example.test/passes", TokenTTL: 72 * time.Hour}
	rootPath := filepath.Join(dir, "test-root.pem")
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	newCertificate := func(certificatePath, keyPath string, serial int64, description string, authority bool, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: description},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), BasicConstraintsValid: true, IsCA: authority,
			KeyUsage: x509.KeyUsageDigitalSignature}
		if authority {
			template.KeyUsage |= x509.KeyUsageCertSign
		} else {
			template.Subject.OrganizationalUnit = []string{cfg.TeamID}
			template.Subject.ExtraNames = []pkix.AttributeTypeAndValue{{Type: asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}, Value: cfg.PassType}}
		}
		if parent == nil {
			parent, parentKey = template, key
		}
		raw, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), parentKey)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Ext(certificatePath) == ".pem" {
			write(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}))
		} else {
			write(certificatePath, raw)
		}
		if keyPath != "" {
			pk, err := x509.MarshalPKCS8PrivateKey(key)
			if err != nil {
				t.Fatal(err)
			}
			write(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}))
		}
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	root, rootKey := newCertificate(rootPath, "", 1, "Fixture Wallet Root", true, nil, nil)
	wwdr, wwdrKey := newCertificate(cfg.WWDRCertificate, "", 2, "Fixture WWDR", true, root, rootKey)
	newCertificate(cfg.Certificate, cfg.PrivateKey, 3, "Fixture Wallet Signer", false, wwdr, wwdrKey)
	for name, size := range map[string]image.Point{"icon.png": image.Pt(29, 29), "icon@2x.png": image.Pt(58, 58), "icon@3x.png": image.Pt(87, 87),
		"logo.png": image.Pt(160, 50), "logo@2x.png": image.Pt(320, 100)} {
		var body bytes.Buffer
		if err := png.Encode(&body, image.NewNRGBA(image.Rectangle{Max: size})); err != nil {
			t.Fatal(err)
		}
		write(filepath.Join(dir, name), body.Bytes())
	}
	return cfg, rootPath, openssl
}

func verifyWalletBundle(t *testing.T, bundle []byte, cfg wallet.Config, root, openssl string, owner int64, secret string) auth.TokenClaims {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range reader.File {
		body, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name] = raw
	}
	const totalFiles = 8
	if len(files) != totalFiles {
		t.Fatalf("package has %d entries", len(files))
	}
	var manifest map[string]string
	if err := json.Unmarshal(files["manifest.json"], &manifest); err != nil || len(manifest) != totalFiles-2 {
		t.Fatalf("manifest parse=%v, entries=%d", err, len(manifest))
	}
	for name, raw := range files {
		if name == "manifest.json" || name == "signature" {
			continue
		}
		hash := sha1.Sum(raw)
		if manifest[name] != hex.EncodeToString(hash[:]) {
			t.Fatalf("manifest does not cover %s", name)
		}
	}
	var pass struct {
		PassType      string            `json:"passTypeIdentifier"`
		TeamID        string            `json:"teamIdentifier"`
		Serial        string            `json:"serialNumber"`
		Barcode       map[string]string `json:"barcode"`
		WebServiceURL string            `json:"webServiceURL"`
		AuthToken     string            `json:"authenticationToken"`
		Expires       string            `json:"expirationDate"`
	}
	if err := json.Unmarshal(files["pass.json"], &pass); err != nil || pass.PassType != cfg.PassType || pass.TeamID != cfg.TeamID ||
		pass.Serial != strconv.FormatInt(owner, 10) || pass.WebServiceURL != cfg.WebServiceURL || pass.Barcode["format"] != "PKBarcodeFormatQR" {
		t.Fatalf("Wallet template does not match configured membership")
	}
	link, err := url.Parse(pass.Barcode["message"])
	if err != nil || link.Host != "members.example.test" || link.Query().Get("lang") != "en" || link.Query().Get("token") != pass.AuthToken {
		t.Fatal("Wallet QR did not retain the configured login URL and token")
	}
	claims := jwt.MapClaims{}
	key := md5.Sum([]byte(secret + ":" + strconv.FormatInt(owner, 10)))
	token, err := jwt.ParseWithClaims(pass.AuthToken, claims, func(*jwt.Token) (any, error) { return []byte(hex.EncodeToString(key[:])), nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithJSONNumber())
	if err != nil || !token.Valid {
		t.Fatal("Wallet JWT signature was invalid")
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	var result auth.TokenClaims
	if err := json.Unmarshal(encoded, &result); err != nil || result.Sub != owner ||
		time.Unix(result.Exp, 0).UTC().Format(time.RFC3339) != pass.Expires {
		t.Fatal("Wallet metadata and bearer credential have different expiries")
	}
	jwtService := auth.NewJWTService(secret)
	if _, err := jwtService.VerifyAccess(pass.AuthToken); err != auth.ErrTokenType {
		t.Fatal("PASS could be used as access token")
	}
	if _, err := jwtService.VerifyRefresh(pass.AuthToken); err != auth.ErrTokenType {
		t.Fatal("PASS could be used as refresh token")
	}
	verifyDir := t.TempDir()
	for _, name := range []string{"manifest.json", "signature", "pass.json"} {
		if err := os.WriteFile(filepath.Join(verifyDir, name), files[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	verify := func(name string) error {
		t.Helper()
		command := exec.Command(openssl, "cms", "-verify", "-binary", "-inform", "DER", "-in", filepath.Join(verifyDir, "signature"),
			"-content", filepath.Join(verifyDir, name), "-CAfile", root, "-purpose", "any")
		return command.Run()
	}
	if err := verify("manifest.json"); err != nil {
		t.Fatalf("OpenSSL could not verify the detached manifest and chain: %v", err)
	}
	if err := verify("pass.json"); err == nil {
		t.Fatal("CMS signed pass.json instead of manifest.json")
	}
	signatureInfo, err := exec.Command(openssl, "cms", "-cmsout", "-print", "-inform", "DER", "-in", filepath.Join(verifyDir, "signature")).Output()
	if err != nil || !bytes.Contains(signatureInfo, []byte("signingTime")) {
		t.Fatal("CMS has no authenticated signing time")
	}
	return result
}
