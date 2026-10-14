package service

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"
)

func TestSystemdRender(t *testing.T) {
	s := &systemd{home: "/home/u"}
	out := s.Render(Spec{Binary: "/home/u/My Apps/rollet", Env: map[string]string{"ROLLET_HOME": "/data/r", "A": "1"}})
	for _, want := range []string{
		`ExecStart="/home/u/My Apps/rollet" serve`,
		"Environment=A=1\nEnvironment=ROLLET_HOME=/data/r\n",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q:\n%s", want, out)
		}
	}
}

func TestSystemdPathHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if p := (&systemd{home: "/home/u"}).Path(); p != "/cfg/systemd/user/claude-rollet.service" {
		t.Errorf("path = %s", p)
	}
}

func TestLaunchdRenderIsValidXML(t *testing.T) {
	l := &launchd{home: "/Users/u"}
	out := l.Render(Spec{Binary: "/Users/u/bin/rollet & co", Env: map[string]string{"ROLLET_HOME": "/x<y>"}})
	dec := xml.NewDecoder(strings.NewReader(out))
	dec.Strict = true
	dec.Entity = xml.HTMLEntity
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not valid XML: %v\n%s", err, out)
		}
	}
	for _, want := range []string{"<string>dev.rollet.claude</string>", "rollet &amp; co", "/x&lt;y&gt;", "<key>KeepAlive</key><true/>", "/Users/u/Library/Logs/claude-rollet.log"} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestLaunchdNoEnvBlockWhenEmpty(t *testing.T) {
	if strings.Contains((&launchd{home: "/u"}).Render(Spec{Binary: "/b"}), "EnvironmentVariables") {
		t.Error("empty env should not render the block")
	}
}
