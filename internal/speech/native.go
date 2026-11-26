package speech

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"

	"github.com/crewlet/crewlet/internal/nativelib"
)

// SherpaVersion is the sherpa-onnx release every platform's library is from.
// A bump changes every pin below and the notices under third_party/ together.
const SherpaVersion = "v2.0.3"

// nativePin is one platform's official sherpa-onnx shared-library archive and
// the two members the recognizer loads: the C API, and onnxruntime beside it
// ($ORIGIN, @loader_path).
type nativePin struct {
	platform string
	archive  ModelArchive
	library  nativelib.File
	runtime  nativelib.File
}

const sherpaReleases = "https://github.com/k2-fsa/sherpa-onnx/releases/download/" + SherpaVersion + "/"

// natives are the release platforms' libraries. linux/arm64 takes the CPU
// build: upstream publishes no TTS-less archive for it.
var natives = []nativePin{{
	platform: "linux/amd64",
	archive: ModelArchive{URL: sherpaReleases + "sherpa-onnx-v2.0.3-linux-x64-shared-no-tts-lib.tar.bz2",
		SHA256: "7e8dcc277dfab8efdb4a3461df46983fa565a95313732ecb4ae1682f0b3ff58d", Size: 9754798},
	library: nativelib.File{Path: "sherpa-onnx-v2.0.3-linux-x64-shared-no-tts-lib/lib/libsherpa-onnx-c-api.so",
		SHA256: "2f0be25cd1e4e6659a0204342f305d7ad7cf8c336ccedb4bec7a343a88c2f91c"},
	runtime: nativelib.File{Path: "sherpa-onnx-v2.0.3-linux-x64-shared-no-tts-lib/lib/libonnxruntime.so",
		SHA256: "4b3607aebd1784b26b6f9b20e4bd974c7ab8287043e4d095cb7d2cb40b5e566e"},
}, {
	platform: "linux/arm64",
	archive: ModelArchive{URL: sherpaReleases + "sherpa-onnx-v2.0.3-linux-aarch64-shared-cpu-lib.tar.bz2",
		SHA256: "e67c22b1d06a20cd5e1577d14be2d80d9be7cf555dc0732b9af3dbc37843a5dc", Size: 12796603},
	library: nativelib.File{Path: "sherpa-onnx-v2.0.3-linux-aarch64-shared-cpu-lib/lib/libsherpa-onnx-c-api.so",
		SHA256: "8e7deb7c9ffbf51193ae08acb1346e9afb20e23044892fa5ef65a391e5918bb8"},
	runtime: nativelib.File{Path: "sherpa-onnx-v2.0.3-linux-aarch64-shared-cpu-lib/lib/libonnxruntime.so",
		SHA256: "cfc800d4438f15e8d9017521326776ca7117d8783cb847ccccc2fc61ac4d540f"},
}, {
	platform: "darwin/amd64",
	archive: ModelArchive{URL: sherpaReleases + "sherpa-onnx-v2.0.3-osx-x64-shared-no-tts-lib.tar.bz2",
		SHA256: "cd652ba19061af79af4c475681ae2a204e413f529d1e696c1d9517ac23214c07", Size: 10005986},
	library: nativelib.File{Path: "sherpa-onnx-v2.0.3-osx-x64-shared-no-tts-lib/lib/libsherpa-onnx-c-api.dylib",
		SHA256: "9fd1c6b0ef0d6612b4e713a98e3d3da29de1083bb8d3f6c8b1937e5ac058395e"},
	runtime: nativelib.File{Path: "sherpa-onnx-v2.0.3-osx-x64-shared-no-tts-lib/lib/libonnxruntime.dylib",
		SHA256: "2bf27180c09e6fd3f407c9ab25205ebf1ff95f2d53bfa35e35409b5849e0ac70"},
}, {
	platform: "darwin/arm64",
	archive: ModelArchive{URL: sherpaReleases + "sherpa-onnx-v2.0.3-osx-arm64-shared-no-tts-lib.tar.bz2",
		SHA256: "fb8854a17d2abd0704217da577a555a6b4b949f509f54f6e5386ded7c74b8818", Size: 8763090},
	library: nativelib.File{Path: "sherpa-onnx-v2.0.3-osx-arm64-shared-no-tts-lib/lib/libsherpa-onnx-c-api.dylib",
		SHA256: "84d44a412dcbdbb9868f1253b6ef7f0e954e3e444d04d8be67c5e5ddc04e7636"},
	runtime: nativelib.File{Path: "sherpa-onnx-v2.0.3-osx-arm64-shared-no-tts-lib/lib/libonnxruntime.dylib",
		SHA256: "3567d114f7299d559993e536d605a6f46d7bc9d2542004accc80ee9bf5457f0b"},
}}

// thisPlatform is the platform this binary runs on, as [natives] names it.
var thisPlatform = runtime.GOOS + "/" + runtime.GOARCH

// nativeFor is platform's library pin, and false where sherpa-onnx publishes
// none this engine pins.
func nativeFor(platform string) (nativePin, bool) {
	for _, p := range natives {
		if p.platform == platform {
			return p, true
		}
	}
	return nativePin{}, false
}

// Name is the library's directory in a prepared directory:
// sherpa-onnx-v2.0.3-linux-amd64.
func (p nativePin) Name() string {
	return "sherpa-onnx-" + SherpaVersion + "-" + path.Dir(p.platform) + "-" + path.Base(p.platform)
}

// files are the members kept, as the native-library cache pins them.
func (p nativePin) files() []nativelib.File { return []nativelib.File{p.library, p.runtime} }

// Library is the C API's path in dir.
func (p nativePin) Library(dir string) string {
	return filepath.Join(dir, path.Base(p.library.Path))
}

// Verify checks the library's members in dir against their pins.
func (p nativePin) Verify(dir string) error { return nativelib.Verify(dir, p.files()) }

// cacheDir is where the library is kept in the native-library cache, under
// the archive's digest so another release is another directory.
func (p nativePin) cacheDir() string {
	return filepath.Join(nativelib.CacheRoot(), "sherpa-onnx", p.archive.SHA256[:8])
}

// Fetch downloads the archive once, from its pinned URL, keeping and verifying
// the two members in dir; a dir that verifies is left as it is. progress hears
// the archive's bytes read.
func (p nativePin) Fetch(ctx context.Context, dir string, progress func(done int64)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("speech: creating the library directory %s: %w", dir, err)
	}
	ctx, release := locked(ctx, dir)
	defer release()
	if p.Verify(dir) == nil {
		return nil
	}
	want := map[string]string{}
	for _, f := range p.files() {
		want[path.Clean(f.Path)] = filepath.Join(dir, path.Base(f.Path))
	}
	if err := p.archive.extract(ctx, want, progress); err != nil {
		return err
	}
	return p.Verify(dir)
}
