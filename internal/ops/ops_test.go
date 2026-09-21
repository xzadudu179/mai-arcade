package ops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xzadudu179/maimai-arcade/internal/model"
	"github.com/xzadudu179/maimai-arcade/internal/service"
)

// testService 构造一个不访问真实网络的服务：上游地址都指向必然失败的本地端口。
func testService(t *testing.T) *service.Service {
	t.Helper()
	svc, err := service.New(service.Options{
		Version:        "1.53",
		Timeout:        time.Second,
		SessionTimeout: time.Second,
		TitleBaseURL:   "http://127.0.0.1:1/Maimai2Servlet/",
		AimeURL:        "http://127.0.0.1:1",
		ChartBaseURL:   "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	return svc
}

// validSGIDText 构造一枚当前有效的二维码文本。
func validSGIDText() string {
	return "SGWCMAID" + time.Now().Format("060102150405") + strings.Repeat("0123456789ABCDEF", 4)
}

// TestRegistryIsComplete 断言每个注册操作都有可用的自描述。
func TestRegistryIsComplete(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("应当注册了操作")
	}
	for _, op := range all {
		if op.Name == "" || op.Run == nil {
			t.Errorf("操作定义不完整: %+v", op)
		}
		if strings.TrimSpace(op.Summary) == "" {
			t.Errorf("操作 %s 缺少用途说明（自描述端点会展示它）", op.Name)
		}
	}
}

// TestRegistryNamesAreSorted 断言操作名顺序稳定。
func TestRegistryNamesAreSorted(t *testing.T) {
	names := Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Names() 未排序: %v", names)
		}
	}
}

// TestExpectedOperationsAreRegistered 断言文档里承诺的操作都在。
func TestExpectedOperationsAreRegistered(t *testing.T) {
	for _, want := range []string{"probe", "verify", "sync", "profile", "b50", "version"} {
		if _, ok := Lookup(want); !ok {
			t.Errorf("缺少操作 %q", want)
		}
	}
}

// TestOnlySyncIsMutating 断言只有同步被标记为会改动外部数据。
//
// 标错会让写保护失效，或者把只读操作误挡在门外。
func TestOnlySyncIsMutating(t *testing.T) {
	for _, op := range All() {
		wantMutating := op.Name == "sync"
		if op.Mutating != wantMutating {
			t.Errorf("操作 %s 的 Mutating = %v, 期望 %v", op.Name, op.Mutating, wantMutating)
		}
	}
}

// TestDispatchRejectsUnknownOperation 断言未知操作返回带提示的参数错误。
func TestDispatchUnknownOperation(t *testing.T) {
	_, err := Dispatch(context.Background(), Deps{Service: testService(t)}, "nope", nil)

	if !errors.Is(err, ErrUnknownOperation) {
		t.Fatalf("错误 = %v, 期望 %v", err, ErrUnknownOperation)
	}
	if !model.IsKind(err, model.KindParam) {
		t.Error("应当是参数类错误")
	}
	if !strings.Contains(err.Error(), "probe") {
		t.Errorf("错误里应列出可用操作: %v", err)
	}
}

// TestDispatchBlocksMutatingWithoutAllowWrite 断言未开启写权限时写操作被拒。
func TestDispatchBlocksMutatingWithoutAllowWrite(t *testing.T) {
	_, err := Dispatch(context.Background(), Deps{Service: testService(t)},
		"sync", json.RawMessage(`{"sgid":"x","credential":"y"}`))

	if !errors.Is(err, ErrWriteDisabled) {
		t.Fatalf("错误 = %v, 期望 %v", err, ErrWriteDisabled)
	}
}

// TestDispatchRejectsInvalidJSON 断言非 JSON 参数被拒。
func TestDispatchRejectsInvalidJSON(t *testing.T) {
	_, err := Dispatch(context.Background(), Deps{Service: testService(t)}, "verify",
		json.RawMessage(`{oops`))

	if !errors.Is(err, ErrBadArguments) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrBadArguments)
	}
}

// TestVerifyRequiresSGID 断言缺少二维码时给出参数错误。
func TestVerifyRequiresSGID(t *testing.T) {
	deps := Deps{Service: testService(t)}

	for _, args := range []string{`{}`, `{"sgid":""}`, `{"sgid":"   "}`} {
		_, err := Dispatch(context.Background(), deps, "verify", json.RawMessage(args))
		if !errors.Is(err, ErrBadArguments) {
			t.Errorf("参数 %s 的错误 = %v, 期望 %v", args, err, ErrBadArguments)
		}
	}
}

// TestErrorsNeverEchoSGID 断言错误信息不回显二维码内容。
//
// 二维码是账号凭证；它出现在错误串里就可能被日志、监控或工单带走。
func TestErrorsNeverEchoSGID(t *testing.T) {
	deps := Deps{Service: testService(t), AllowWrite: true}

	// 用一段格式合法、带可识别标记的二维码，检查标记是否出现在任何错误串里。
	const marker = "DEADBEEF"
	sgid := "SGWCMAID" + time.Now().Format("060102150405") + strings.Repeat(marker, 8)

	calls := []struct {
		op   string
		args string
	}{
		{"verify", `{"sgid":"` + sgid + `"}`},
		{"profile", `{"sgid":"` + sgid + `"}`},
		{"b50", `{"sgid":"` + sgid + `"}`},
		{"sync", `{"sgid":"` + sgid + `","credential":"token"}`},
	}

	for _, call := range calls {
		t.Run(call.op, func(t *testing.T) {
			_, err := Dispatch(context.Background(), deps, call.op, json.RawMessage(call.args))
			if err == nil {
				t.Skip("上游不可用但也未报错，跳过")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("错误信息泄露了二维码内容: %v", err)
			}
		})
	}
}

// TestVersionOperationDescribesItself 断言自描述操作返回操作清单与错误码目录。
func TestVersionOperationDescribesItself(t *testing.T) {
	result, err := Dispatch(context.Background(), Deps{Service: testService(t)}, "version", nil)
	if err != nil {
		t.Fatalf("version 不应报错: %v", err)
	}

	payload, ok := result.(versionResult)
	if !ok {
		t.Fatalf("返回类型 = %T, 期望 versionResult", result)
	}
	if payload.Version != "1.53" {
		t.Errorf("版本 = %q, 期望 1.53", payload.Version)
	}
	if len(payload.Ops) != len(All()) {
		t.Errorf("自描述操作数 = %d, 期望 %d", len(payload.Ops), len(All()))
	}
}

// TestB50RejectsNegativeLimit 断言非法参数被拒。
func TestB50RejectsNegativeLimit(t *testing.T) {
	_, err := Dispatch(context.Background(), Deps{Service: testService(t)}, "b50",
		json.RawMessage(`{"sgid":"`+validSGIDText()+`","limit":-1}`))

	if !errors.Is(err, ErrBadArguments) {
		t.Errorf("错误 = %v, 期望 %v", err, ErrBadArguments)
	}
}

// TestDescribeMatchesRegistry 断言自描述与注册表一致。
func TestDescribeMatchesRegistry(t *testing.T) {
	described := Describe()
	if len(described) != len(All()) {
		t.Fatalf("自描述条数 = %d, 期望 %d", len(described), len(All()))
	}
	for i, item := range described {
		if item.Name != All()[i].Name {
			t.Errorf("第 %d 条名称不符: %q vs %q", i, item.Name, All()[i].Name)
		}
	}
}

// TestDuplicateRegisterPanics 断言重名注册立刻暴露为编码错误。
func TestDuplicateRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("重名注册应当 panic")
		}
	}()
	Register(Operation{Name: "probe", Summary: "重复", Run: func(context.Context, Deps, json.RawMessage) (any, error) {
		return nil, nil
	}})
}

// TestRegisterRejectsIncomplete 断言不完整的注册被拒。
func TestRegisterRejectsIncomplete(t *testing.T) {
	tests := []struct {
		name string
		op   Operation
	}{
		{"缺少名字", Operation{Run: func(context.Context, Deps, json.RawMessage) (any, error) { return nil, nil }}},
		{"缺少实现", Operation{Name: "x"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("不完整的注册应当 panic")
				}
			}()
			Register(tc.op)
		})
	}
}

// TestAllCodesUsedByOpsAreInCatalog 断言 ops 自己的错误码都进了对外目录。
func TestAllCodesUsedByOpsAreInCatalog(t *testing.T) {
	for _, sentinel := range []*model.Error{ErrUnknownOperation, ErrWriteDisabled, ErrBadArguments} {
		if _, ok := model.LookupCode(sentinel.Sentinel); !ok {
			t.Errorf("错误码 %s 未登记进目录", sentinel.Sentinel)
		}
	}
}
