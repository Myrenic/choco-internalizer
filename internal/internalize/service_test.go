package internalize

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lihongjie0209/choco-internalizer/internal/archive"
	"github.com/lihongjie0209/choco-internalizer/internal/powershell"
)

type staticDownloadClient map[string]string

func (c staticDownloadClient) Do(request *http.Request) (*http.Response, error) {
	body, ok := c[request.URL.String()]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Body:       io.NopCloser(strings.NewReader("not found")),
			Request:    request,
		}, nil
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(strings.NewReader(body)),
		Request:       request,
	}, nil
}

func TestPrepareInstallScriptInternalizesFirefoxLocale(t *testing.T) {
	t.Parallel()
	script := `$locale = 'en-US' # default used by the package updater
$locale = GetLocale -localeFile "$toolsPath\LanguageChecksums.csv"
$builds = @{
  'x86' = @{ Url = "https://download.mozilla.org/?product=firefox-155.0.1-ssl&os=win&lang=${locale}" }
  'x64' = @{ Url = "https://download.mozilla.org/?product=firefox-155.0.1-ssl&os=win64&lang=${locale}" }
}
Install-ChocolateyPackage -Url $builds.x64.Url`

	prepared := prepareInstallScript("Firefox", script)
	if strings.Contains(prepared, "$locale = GetLocale") {
		t.Fatalf("prepareInstallScript() retained runtime locale selection:\n%s", prepared)
	}
	rewritten, resources, err := powershell.Rewrite(prepared)
	if err != nil {
		t.Fatalf("Rewrite() error = %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("Rewrite() resources = %#v, want two architecture installers", resources)
	}
	if strings.Contains(rewritten, "download.mozilla.org") {
		t.Fatalf("Rewrite() retained Firefox download URL:\n%s", rewritten)
	}
}

func TestPrepareInstallScriptDoesNotChangeOtherPackages(t *testing.T) {
	t.Parallel()
	script := "$locale = GetLocale"
	if got := prepareInstallScript("thunderbird", script); got != script {
		t.Fatalf("prepareInstallScript() = %q, want unchanged script", got)
	}
}

func TestRunInternalizesURLsFromDotSourcedDataScript(t *testing.T) {
	t.Parallel()
	const (
		x86URL = "https://downloads.example.test/VC_redist.x86.exe"
		x64URL = "https://downloads.example.test/VC_redist.x64.exe"
	)
	input := filepath.Join(t.TempDir(), "vcredist140.nupkg")
	output := filepath.Join(t.TempDir(), "vcredist140.internalized.nupkg")
	pkg := archive.Package{Files: map[string][]byte{
		"vcredist140.nuspec": []byte(`<?xml version="1.0"?><package><metadata><id>vcredist140</id><version>14.51.36247</version><authors>Microsoft</authors><description>test</description></metadata></package>`),
		"tools/chocolateyInstall.ps1": []byte(`. (Join-Path $PSScriptRoot 'data.ps1')
Install-ChocolateyPackage -PackageName 'vcredist140' @installData32
Install-ChocolateyPackage -PackageName 'vcredist140' @installData64`),
		"tools/data.ps1": []byte(`$installData32 = @{
  Url = 'https://downloads.example.test/VC_redist.x86.exe'
  Checksum = 'unused'
}
$installData64 = @{
  Url64 = 'https://downloads.example.test/VC_redist.x64.exe'
  Checksum64 = 'unused'
}`),
	}}
	if err := pkg.Write(input); err != nil {
		t.Fatalf("write input package: %v", err)
	}

	report, err := New(staticDownloadClient{
		x86URL: "x86 installer",
		x64URL: "x64 installer",
	}, Options{}).Run(context.Background(), input, output)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(report.Resources) != 2 {
		t.Fatalf("Run() resources = %#v, want two installers", report.Resources)
	}

	got, err := archive.Read(output)
	if err != nil {
		t.Fatalf("read output package: %v", err)
	}
	installScript := string(got.Files["tools/chocolateyInstall.ps1"])
	dataScript := string(got.Files["tools/data.ps1"])
	if strings.Contains(installScript, "Install-ChocolateyPackage") ||
		!strings.Contains(installScript, "Install-ChocolateyInstallPackage") {
		t.Fatalf("install script retained network helper:\n%s", installScript)
	}
	for _, externalURL := range []string{x86URL, x64URL} {
		if strings.Contains(dataScript, externalURL) {
			t.Fatalf("data script retained %s:\n%s", externalURL, dataScript)
		}
	}
	for _, expected := range []string{"file =", "file64 ="} {
		if !strings.Contains(dataScript, expected) {
			t.Errorf("data script missing %q:\n%s", expected, dataScript)
		}
	}
	for _, embedded := range []string{
		"tools/VC_redist.x86.exe",
		"tools/VC_redist.x64.exe",
	} {
		if _, ok := got.Files[embedded]; !ok {
			t.Errorf("output package missing %s", embedded)
		}
	}
}
