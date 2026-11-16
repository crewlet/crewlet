package host_test

import (
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

// downloadRawBytes is BenchmarkDownload's file: incompressible, and under the
// default code.limits.max_blob_bytes.
const downloadRawBytes = 32 << 20

// BenchmarkDownload is what a download costs the node beside a clone of the
// same files, warm: kind=raw a 32 MiB incompressible file (`cat-file blob`),
// kind=archive the 10 000-file tree of the 200-push tree fixture (3 000
// random bytes a file, 30 MB), and kind=clone that tree cloned. Each reports
// MB/s of files and the node's git CPU a MiB of files (cpu-s/MiB), the
// figure ArchiveFreeBytes and the archive places rest on.
func BenchmarkDownload(b *testing.B) {
	gitlettest.Quiet(b)
	f, p := benchFleet(b, memobj.New(), 1)
	node := f.Nodes()[0]
	url := repoURL(node)
	commits, _ := treeFixture(b, p, url, 200)
	tip := commits[len(commits)-1]
	const treeBytes = 200 * treeFilesPerPush * treeFileBytes
	work := p.Init()
	noise := p.Commit(work, map[string]string{"noise": randomBytes(downloadRawBytes)}, "noise")
	mustPush(b, p, work, url, "HEAD:refs/heads/noise")
	token := f.Token(b, "ceo")
	get := func(b *testing.B, target string) int64 {
		b.Helper()
		req, err := http.NewRequestWithContext(b.Context(), http.MethodGet, node.URL()+target, nil)
		if err != nil {
			b.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := node.Client().Do(req)
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		n, err := io.Copy(io.Discard, resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK {
			b.Fatalf("GET %s answered %d after %d bytes: %v", target, resp.StatusCode, n, err)
		}
		return n
	}
	perMiB := func(b *testing.B, cpuSeconds float64, files int64) {
		b.ReportMetric(cpuSeconds/float64(b.N)/(float64(files)/objstore.MiB), "cpu-s/MiB")
	}
	b.Run("kind=raw/size=32MiB", func(b *testing.B) {
		cpu := host.GitCPU(node.Host(), "cat-file", "blob")
		b.SetBytes(downloadRawBytes)
		for b.Loop() {
			get(b, rawPath(noise, "noise"))
		}
		perMiB(b, cpu().Seconds(), downloadRawBytes)
	})
	b.Run("kind=archive/files=10000", func(b *testing.B) {
		cpu := host.GitCPU(node.Host(), "archive")
		b.SetBytes(treeBytes)
		var zipped int64
		for b.Loop() {
			zipped = get(b, zipPath(tip))
		}
		perMiB(b, cpu().Seconds(), treeBytes)
		b.ReportMetric(float64(zipped)/float64(treeBytes), "zip/files")
	})
	b.Run("kind=clone/files=10000", func(b *testing.B) {
		b.SetBytes(treeBytes)
		var cpuMs float64
		for b.Loop() {
			before := serverCPU(node.Metrics())
			dir := p.Clone(url)
			cpuMs += serverCPU(node.Metrics()) - before
			b.StopTimer()
			if err := os.RemoveAll(dir); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		perMiB(b, cpuMs/1000, treeBytes)
	})
}
