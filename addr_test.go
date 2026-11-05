package cho

import (
	"net/http"
	"strings"
	"testing"
)

// Start 必须尊重调用方给的 Addr。
//
// 原来这里无条件覆盖成 ":port"（监听所有网卡），而配置里写的是 127.0.0.1 ——
// 表现是"配置文件看着对、实际在监听 0.0.0.0"，靠 systemd 的
// IPAddressDeny 才没暴露，而那个沙箱顺带把出网也掐了。
func TestStartRespectsAddr(t *testing.T) {
	c := newTestApp()
	srv, _, err := c.Start(0, &http.Server{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if host, _, _ := strings.Cut(srv.Addr, ":"); host == "" || host == "[::]" {
		t.Fatalf("监听地址应该是 127.0.0.1，实际 %q", srv.Addr)
	}
	if !strings.HasPrefix(srv.Addr, "127.0.0.1:") {
		t.Fatalf("监听地址应该是 127.0.0.1:xxx，实际 %q", srv.Addr)
	}
}

// 没给 Addr 时沿用 ":port"。
func TestStartDefaultsToAllInterfaces(t *testing.T) {
	c := newTestApp()
	srv, _, err := c.Start(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Addr == "" {
		t.Fatal("应该有地址")
	}
}

// 只给了主机名（没带端口）时补上 port。
func TestStartHostOnlyGetsPort(t *testing.T) {
	c := newTestApp()
	srv, _, err := c.Start(0, &http.Server{Addr: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !strings.HasPrefix(srv.Addr, "127.0.0.1:") {
		t.Fatalf("应该补上端口，实际 %q", srv.Addr)
	}
}
