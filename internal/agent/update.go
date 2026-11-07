package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// Version agent 版本；只有修改 agent 时才提升，平台发版不会让已安装的 agent 无谓升级
const Version = "1.3.2"

// releasePublicKey 发布签名公钥。私钥只在发布机器上，服务器被入侵也无法向用户设备推送程序
var releasePublicKey = mustDecodeKey("E6Z9n9b4UGZMCIRhnxsksYl/tuuivUBGqkCJbTeVsGI=")

func mustDecodeKey(s string) ed25519.PublicKey {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != ed25519.PublicKeySize {
		panic("invalid release public key")
	}
	return key
}

// Manifest 由发布脚本生成并签名：/probe/manifest.json 与 /probe/manifest.json.sig
type Manifest struct {
	Version string                  `json:"version"`
	Files   map[string]ManifestFile `json:"files"` // GOOS-GOARCH => 文件
}

type ManifestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func Platform() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}

// VerifyManifest 校验签名后再解析，未签名或签名不符的清单一律拒绝
func VerifyManifest(data, signature []byte, key ed25519.PublicKey) (*Manifest, error) {
	sig, err := base64.StdEncoding.DecodeString(string(trimSpace(signature)))
	if err != nil || !ed25519.Verify(key, data, sig) {
		return nil, errors.New("manifest signature is invalid")
	}
	var manifest = &Manifest{}
	if err = json.Unmarshal(data, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

type Updater struct {
	Server string
	HTTP   *http.Client
	Key    ed25519.PublicKey
	// Executable 要替换的程序路径，默认为当前程序
	Executable string
}

func NewUpdater(server string) *Updater {
	return &Updater{Server: server, HTTP: &http.Client{Transport: ipv4Transport()}, Key: releasePublicKey}
}

// Update 下载并替换为清单中的版本；返回 true 表示已替换，需要重启
func (u *Updater) Update(ctx context.Context) (bool, error) {
	data, err := u.get(ctx, "/probe/manifest.json", 1<<20)
	if err != nil {
		return false, err
	}
	sig, err := u.get(ctx, "/probe/manifest.json.sig", 4096)
	if err != nil {
		return false, err
	}
	manifest, err := VerifyManifest(data, sig, u.Key)
	if err != nil {
		return false, err
	}
	if manifest.Version == Version {
		return false, nil
	}
	file, ok := manifest.Files[Platform()]
	if !ok || file.Name == "" || filepath.Base(file.Name) != file.Name {
		return false, fmt.Errorf("no release for %s", Platform())
	}
	binary, err := u.get(ctx, "/probe/"+file.Name, 64<<20)
	if err != nil {
		return false, err
	}
	var sum = sha256.Sum256(binary)
	if hex.EncodeToString(sum[:]) != file.SHA256 || int64(len(binary)) != file.Bytes {
		return false, errors.New("downloaded binary does not match the signed manifest")
	}
	return true, u.replace(binary)
}

func (u *Updater) replace(binary []byte) error {
	var exe = u.Executable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	var next = exe + ".new"
	if err := os.WriteFile(next, binary, 0755); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// 运行中的程序不能覆盖，但可以改名
		_ = os.Remove(exe + ".old")
		if err := os.Rename(exe, exe+".old"); err != nil {
			return err
		}
	}
	return os.Rename(next, exe)
}

func (u *Updater) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL(u.Server, path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "edge-probe/"+Version)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d", path, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: response too large", path)
	}
	return data, nil
}
