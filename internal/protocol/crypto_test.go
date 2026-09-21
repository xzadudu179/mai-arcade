package protocol

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestAPIHashOfficialVectors 用文档 §2.4 的三条官方向量断言 api_hash 算法。
//
// 这三条是全项目唯一有官方背书的算法证据，其余协议细节都建立在它之上。
func TestAPIHashOfficialVectors(t *testing.T) {
	const obfuscate150 = "B44df8yT"

	tests := []struct {
		name    string
		apiName string
		want    string
	}{
		{"Ping 官方向量", "Ping", "250b3482854e7697de7d8eb6ea1fabb1"},
		{"GetUserPreviewApi 官方向量", "GetUserPreviewApi", "004cf848f96d393a5f2720101e30b93d"},
		{"GetUserDataApi 官方向量", "GetUserDataApi", "3af1e5b298bb5b7379c94934b2e038c5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := apiHash(tc.apiName, obfuscate150); got != tc.want {
				t.Errorf("apiHash(%q) = %s, 期望 %s", tc.apiName, got, tc.want)
			}
		})
	}
}

// TestAPIHashVersionTable 断言 1.53 与 1.55 下的常用 hash，供线上调试对照。
func TestAPIHashVersionTable(t *testing.T) {
	tests := []struct {
		apiName string
		v153    string
		v155    string
	}{
		{"Ping", "5c39ae037195be3f0f9a8e8f6f8ec849", "3bae8c1162b3148abcfc38a89c03e3e7"},
		{"UserLoginApi", "f9d6e44dd1a72ff9ff56efbc2f77fe8b", "04dd218d2070685e728719b974f9b8c0"},
		{"GetUserMusicApi", "9497365ea1a6caf7789cee131dfd3017", "0b31f4a74d0b4eb07ef53e036625ed89"},
		{"UserLogoutApi", "4329b8dc275ea277243c26185bee35c7", "aaa0817a628ae9cc3df35b120ecb33a4"},
	}

	for _, tc := range tests {
		t.Run(tc.apiName, func(t *testing.T) {
			if got := apiHash(tc.apiName, Versions["1.53"].ObfuscateParam); got != tc.v153 {
				t.Errorf("1.53 apiHash(%q) = %s, 期望 %s", tc.apiName, got, tc.v153)
			}
			if got := apiHash(tc.apiName, Versions["1.55"].ObfuscateParam); got != tc.v155 {
				t.Errorf("1.55 apiHash(%q) = %s, 期望 %s", tc.apiName, got, tc.v155)
			}
		})
	}
}

// TestVersionsTableComplete 断言参数表里每个版本的四项参数齐全且长度正确。
//
// 这条测试的价值在于抄错一个字符就会失败：Key/IV 长度断言能直接抓出转录错误。
func TestVersionsTableComplete(t *testing.T) {
	for _, name := range SupportedVersions() {
		t.Run(name, func(t *testing.T) {
			v, err := LookupVersion(name)
			if err != nil {
				t.Fatalf("LookupVersion(%q) 报错: %v", name, err)
			}
			if v.Encoding != name {
				t.Errorf("Encoding = %q, 期望与键名一致 %q", v.Encoding, name)
			}
			if err := v.Validate(); err != nil {
				t.Errorf("Validate() 报错: %v", err)
			}
			if len(v.Key) != 32 {
				t.Errorf("Key 长度 = %d, 期望 32", len(v.Key))
			}
			if len(v.IV) != 16 {
				t.Errorf("IV 长度 = %d, 期望 16", len(v.IV))
			}
		})
	}

	if _, ok := Versions[DefaultVersion]; !ok {
		t.Errorf("默认版本 %q 不在参数表里", DefaultVersion)
	}
	if _, err := LookupVersion("9.99"); err == nil {
		t.Error("未知版本应当报错")
	}
	if v, err := LookupVersion(""); err != nil || v.Encoding != DefaultVersion {
		t.Errorf("空版本号应回落到默认版本，得到 %v / %v", v.Encoding, err)
	}
}

// TestPackUnpackRoundTrip 断言各版本的打包→解包往返一致。
//
// 1.53 起是「先压缩后加密」，1.40–1.52 相反，两种顺序都必须自洽。
func TestPackUnpackRoundTrip(t *testing.T) {
	payload := map[string]any{
		"userId":    10807675,
		"nextIndex": 0,
		"note":      "中文与符号 <>[]{} 也要能往返",
	}

	for _, name := range SupportedVersions() {
		t.Run(name, func(t *testing.T) {
			v := Versions[name]

			packed, err := packBody(payload, v)
			if err != nil {
				t.Fatalf("packBody 报错: %v", err)
			}
			if len(packed)%16 != 0 {
				t.Errorf("密文长度 %d 不是 16 的整数倍", len(packed))
			}

			plain, err := unpackBody(packed, v)
			if err != nil {
				t.Fatalf("unpackBody 报错: %v", err)
			}
			if !strings.Contains(string(plain), "10807675") {
				t.Errorf("解出的明文不含预期内容: %s", plain)
			}
		})
	}
}

// TestUnpackAcceptsPlainJSON 断言响应没有 zlib 头时也能解出明文。
//
// 部分响应不带压缩头，无条件解压会把它们全部判成「解密失败」，
// 从而把正常的协议版本误报成版本不匹配。
func TestUnpackAcceptsPlainJSON(t *testing.T) {
	for _, name := range []string{"1.53", "1.55"} {
		t.Run(name, func(t *testing.T) {
			v := Versions[name]
			plain := []byte(`{"returnCode":1}`)

			cipherText, err := aesCBCEncrypt(plain, v.Key, v.IV)
			if err != nil {
				t.Fatalf("加密失败: %v", err)
			}
			got, err := unpackBody(cipherText, v)
			if err != nil {
				t.Fatalf("unpackBody 报错: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Errorf("明文 = %s, 期望 %s", got, plain)
			}
		})
	}
}

// TestUnpackWrongKeyFails 断言换错版本参数时必须报错，而不是解出垃圾。
//
// probe 正是靠这个信号把「版本不匹配」和「业务错误」区分开。
func TestUnpackWrongKeyFails(t *testing.T) {
	packed, err := packBytes([]byte(`{"returnCode":1}`), Versions["1.53"])
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}

	_, err = unpackBody(packed, Versions["1.55"])
	if err == nil {
		t.Fatal("用错误的密钥解包应当报错")
	}
	if !errors.Is(err, ErrDecrypt) && !errors.Is(err, ErrVersionMismatch) {
		t.Errorf("错误应属于解密类，得到 %v", err)
	}
}

// TestPKCS7UnpadRejectsInvalid 断言非法填充被拒绝，让错误密钥无法蒙混过关。
func TestPKCS7UnpadRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"零填充字节", append(bytes.Repeat([]byte{0x41}, 15), 0x00)},
		{"填充字节大于块大小", append(bytes.Repeat([]byte{0x41}, 15), 0x20)},
		{"填充内容不一致", append(bytes.Repeat([]byte{0x41}, 13), 0x03, 0x03, 0x02)},
		{"长度非块大小整数倍", []byte{0x01, 0x02, 0x03}},
		{"空输入", []byte{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := pkcs7Unpad(tc.data, 16); err == nil {
				t.Error("非法填充应当报错")
			}
		})
	}
}

// TestPKCS7PadUnpadRoundTrip 断言填充与去填充互逆，含空输入与整块边界。
func TestPKCS7PadUnpadRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 15, 16, 17, 31, 32} {
		original := bytes.Repeat([]byte{0x5A}, size)
		padded := pkcs7Pad(original, 16)
		if len(padded)%16 != 0 {
			t.Errorf("长度 %d 填充后 %d 不是块大小整数倍", size, len(padded))
		}
		got, err := pkcs7Unpad(padded, 16)
		if err != nil {
			t.Errorf("长度 %d 去填充报错: %v", size, err)
			continue
		}
		if !bytes.Equal(got, original) {
			t.Errorf("长度 %d 往返不一致", size)
		}
	}
}

// TestHasZlibHeader 断言 zlib 魔数嗅探不会把明文误判为压缩数据。
func TestHasZlibHeader(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{"真实 zlib 头", []byte{0x78, 0x9c, 0x00}, true},
		{"zlib 低压缩头", []byte{0x78, 0x01, 0x00}, true},
		{"JSON 明文", []byte(`{"a":1}`), false},
		{"过短", []byte{0x78}, false},
		{"首字节不符", []byte{0x79, 0x9c}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasZlibHeader(tc.data); got != tc.want {
				t.Errorf("hasZlibHeader(%v) = %v, 期望 %v", tc.data, got, tc.want)
			}
		})
	}
}
