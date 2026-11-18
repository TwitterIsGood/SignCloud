package control

import (
	"bytes"
	"html"
	"html/template"
	"strings"
	"testing"
	"time"
)

func TestInstallPagePreservesITMSServicesURL(t *testing.T) {
	const otaURL = "itms-services://?action=download-manifest&url=https%3A%2F%2Fexample.com%2Fmanifest.plist"
	data := installPageData{
		Build: Build{
			Name:        "Example",
			Kind:        "xcode",
			Status:      "succeeded",
			UpdatedAt:   time.Unix(0, 0),
			QRURL:       "https://example.com/qr.png",
			ArtifactURL: "https://example.com/app.ipa",
		},
		SafeOTAURL: template.URL(otaURL),
	}

	var rendered bytes.Buffer
	if err := template.Must(template.New("install").Parse(installPage)).Execute(&rendered, data); err != nil {
		t.Fatal(err)
	}
	page := rendered.String()
	if strings.Contains(page, "#ZgotmplZ") {
		t.Fatal("OTA URL was rejected by html/template")
	}
	if !strings.Contains(html.UnescapeString(page), `href="`+otaURL+`"`) {
		t.Fatalf("OTA URL missing from install page: %s", page)
	}
}

func TestInstallPageShowsProgressWhileRunning(t *testing.T) {
	build := Build{
		Name:         "Example",
		Kind:         "xcode",
		Status:       "running",
		InstallToken: "token",
		CreatedAt:    time.Unix(0, 0),
		UpdatedAt:    time.Unix(90, 0),
		Logs:         "$ xcodebuild -project Example.xcodeproj build\n",
	}
	var rendered bytes.Buffer
	if err := template.Must(template.New("install").Parse(installPage)).Execute(&rendered, installPageData{
		Build:       build,
		StatusText:  buildStatusText(build.Status),
		Progress:    buildProgress(build),
		LogsTail:    tailText(build.Logs, installLogTailLimit),
		ElapsedText: elapsedText(build),
	}); err != nil {
		t.Fatal(err)
	}
	page := rendered.String()
	for _, marker := range []string{
		`data-status="running"`,
		`data-status-url="/install/token/status"`,
		`id="progress"`,
		`id="track"`,
		`id="details"`,
		`stage current`,
		`构建进度`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("running install page missing %q", marker)
		}
	}
	if strings.Contains(page, "保存") || strings.Contains(page, "安装到 iPhone") {
		t.Fatal("running install page should not offer install actions")
	}
}

func TestBuildProgressStages(t *testing.T) {
	running := buildProgress(Build{Kind: "xcode", Status: "running", Stage: "building"})
	if running.Percent == nil || *running.Percent != 55 {
		t.Fatalf("unexpected percent: %+v", running.Percent)
	}
	if len(running.Stages) != 4 || running.Stages[0].State != "done" || running.Stages[1].State != "current" {
		t.Fatalf("unexpected stages: %+v", running.Stages)
	}
	failed := buildProgress(Build{Kind: "xcode", Status: "failed", Stage: "building"})
	if failed.Stages[1].State != "failed" {
		t.Fatalf("failed stage not marked: %+v", failed.Stages)
	}
	inferred := buildProgress(Build{Kind: "xcode", Status: "running", Logs: "$ xcodebuild build\n"})
	if inferred.Stage != "building" {
		t.Fatalf("log inference failed: %q", inferred.Stage)
	}
	macos := buildProgress(Build{Kind: "macos", Status: "running", Stage: "notarizing"})
	if macos.Percent == nil || *macos.Percent != 82 || len(macos.Stages) != 9 {
		t.Fatalf("unexpected macos progress: %+v", macos)
	}
}

func TestInstallPageContainsOverflowGuards(t *testing.T) {
	for _, rule := range []string{
		"grid-template-columns:minmax(280px,340px) minmax(0,1fr)",
		".info{min-width:0",
		".facts strong{min-width:0;overflow-wrap:anywhere",
		"html,body{max-width:100%;overflow-x:hidden}",
	} {
		if !strings.Contains(installPage, rule) {
			t.Fatalf("install page missing overflow guard %q", rule)
		}
	}
}
