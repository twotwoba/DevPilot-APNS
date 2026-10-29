// test-push 子命令:真机验证的触发工具。
//
// 用 daemon 凭据连 relay WSS,注入一条测试事件,走完整链路
// daemon→relay→APNs→iPhone;若 App 侧作出决策,还会收到回流的 Decision。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/internal/id"
	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

func runTestPush(args []string) int {
	fs := flag.NewFlagSet("test-push", flag.ContinueOnError)
	relay := fs.String("relay", "", "relay base URL (defaults to PUBLIC_BASE_URL env var)")
	cred := fs.String("credential", "", "daemon credential dpd_... (required)")
	kind := fs.String("kind", "approval", "event kind: approval | task")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the app decision after Ack")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	base := strings.TrimRight(*relay, "/")
	if base == "" {
		base = strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/")
	}
	if base == "" || *cred == "" {
		fmt.Fprintln(os.Stderr, "--relay (or PUBLIC_BASE_URL) and --credential are required")
		return 2
	}
	daemonID, _, ok := pairing.ParseCredential(*cred, pairing.DaemonPrefix, pairing.DaemonIDPrefix)
	if !ok {
		fmt.Fprintln(os.Stderr, "--credential is not a valid daemon credential (dpd_)")
		return 2
	}
	env, err := buildTestEnvelope(daemonID, *kind)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	// 总预算 = 握手/投递 30s + 决策等待。
	ctx, cancel := context.WithTimeout(context.Background(), *wait+30*time.Second)
	defer cancel()

	wsURL := strings.Replace(base, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1) + "/ws/daemon"
	fmt.Printf("connecting to %s ...\n", wsURL)
	conn, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader:      http.Header{"Authorization": {"Bearer " + *cred}},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		fmt.Fprintf(os.Stderr, "connect failed (HTTP %d): %v\n", status, err)
		return 1
	}
	defer conn.CloseNow()

	hello := wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: id.New(),
		Role: wire.RoleDaemon, DaemonID: daemonID, TS: time.Now().Unix(),
	}
	if err := writeMsg(ctx, conn, hello); err != nil {
		fmt.Fprintf(os.Stderr, "failed to send hello: %v\n", err)
		return 1
	}
	ackMsg, err := readMsg(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handshake failed: %v\n", err)
		return 1
	}
	if _, ok := ackMsg.(*wire.HelloAck); !ok {
		fmt.Fprintln(os.Stderr, "handshake failed: first response is not hello_ack")
		return 1
	}

	if err := writeMsg(ctx, conn, *env); err != nil {
		fmt.Fprintf(os.Stderr, "failed to send event: %v\n", err)
		return 1
	}
	fmt.Printf("sent %s event event_id=%s approval_id=%s\n",
		env.Type, env.EventID, env.ApprovalID)

	// 先等 Ack;accepted 后继续等决策回流(手机上点一下,这里能看到)。
	acked := false
	for {
		msg, err := readMsg(ctx, conn)
		if err != nil {
			if acked {
				fmt.Println("timed out or connection closed while waiting for decision (check the iPhone to see whether the push arrived)")
				return 0
			}
			fmt.Fprintf(os.Stderr, "no ack received: %v\n", err)
			return 1
		}
		switch m := msg.(type) {
		case *wire.Ack:
			if m.RefMsgID != env.MsgID {
				continue
			}
			if m.Status != wire.AckAccepted {
				fmt.Fprintf(os.Stderr, "relay rejected event: %s (quota gate or invalid credential)\n", m.Status)
				return 1
			}
			acked = true
			fmt.Printf("relay accepted the event; waiting for app decision (up to %s, Ctrl-C to exit)...\n", *wait)
			fmt.Println("→ check the iPhone lock screen notification and tap approve/deny")
		case *wire.Decision:
			fmt.Printf("decision received: %s (approval_id=%s)\n", m.Decision, m.ApprovalID)
			return 0
		}
	}
}

// buildTestEnvelope 构造测试事件;payload 只用摘要白名单字段。
func buildTestEnvelope(daemonID, kind string) (*wire.Envelope, error) {
	now := time.Now().Unix()
	env := &wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent,
		MsgID:     id.New(),
		EventID:   "ev_testpush_" + id.New(),
		Agent:     wire.AgentClaude,
		DaemonID:  daemonID,
		SessionID: "sess_testpush_" + id.New(),
		TS:        now,
	}
	switch kind {
	case "approval":
		env.Type = wire.EventApprovalRequired
		env.ApprovalID = "apv_testpush_" + id.New()
		env.Payload = wire.Payload{
			SessionTitle:   "DevPilot Test Push",
			ToolName:       "Bash",
			CommandSummary: "echo test-push",
		}
	case "task":
		env.Type = wire.EventTaskComplete
		env.Payload = wire.Payload{
			SessionTitle: "DevPilot Test Push",
			ReplySummary: "Test push completed",
		}
	default:
		return nil, fmt.Errorf("--kind must be approval or task")
	}
	return env, nil
}

func writeMsg(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := wire.Encode(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(wctx, websocket.MessageText, data)
}

func readMsg(ctx context.Context, conn *websocket.Conn) (any, error) {
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, data, err := conn.Read(rctx)
	if err != nil {
		return nil, err
	}
	return wire.Decode(data)
}
