package protocol

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// obfuscateSuffix 是所有版本共用的后缀，拼在 ObfuscateParam 之前参与 api_hash 计算。
const obfuscateSuffix = "MaimaiChn"

// apiHash 计算标题服务器路径中的不透明标识：hex(md5(apiName + "MaimaiChn" + obfuscateParam))，小写。
//
// apiName 必须与官方变体名逐字符一致，否则算出的 hash 对不上，服务器会当作未知路径处理。
func apiHash(apiName, obfuscateParam string) string {
	sum := md5.Sum([]byte(apiName + obfuscateSuffix + obfuscateParam))
	return hex.EncodeToString(sum[:])
}

// packBody 把请求结构体编码成机台可接受的请求体。
//
// 1.53 起改为先 zlib 压缩再 AES 加密，与 1.40–1.52 的顺序相反，顺序由版本参数决定。
func packBody(payload any, v Version) ([]byte, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体: %w", err)
	}
	return packBytes(plain, v)
}

// packBytes 对已序列化的明文执行版本规定的压缩/加密顺序。
func packBytes(plain []byte, v Version) ([]byte, error) {
	var toEncrypt []byte
	if v.ZlibBeforeEncrypt {
		var err error
		if toEncrypt, err = zlibCompress(plain); err != nil {
			return nil, err
		}
	} else {
		toEncrypt = plain
	}

	cipherText, err := aesCBCEncrypt(toEncrypt, v.Key, v.IV)
	if err != nil {
		return nil, err
	}

	if v.ZlibBeforeEncrypt {
		return cipherText, nil
	}
	return zlibCompress(cipherText)
}

// unpackBody 把响应体还原成明文 JSON。
//
// 响应可能是「压缩过」也可能是「纯明文 JSON」——1.55 的部分响应不带 zlib 头，
// 因此按魔数嗅探而不是无条件解压。
func unpackBody(body []byte, v Version) ([]byte, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: 响应体为空", ErrEmptyResponse)
	}

	if v.ZlibBeforeEncrypt {
		plain, err := aesCBCDecrypt(body, v.Key, v.IV)
		if err != nil {
			return nil, err
		}
		return maybeInflate(plain)
	}

	// 1.40–1.52：先解压后解密。
	raw, err := maybeInflate(body)
	if err != nil {
		return nil, err
	}
	return aesCBCDecrypt(raw, v.Key, v.IV)
}

// zlibCompress 用 zlib 默认压缩级别打包，产出以 0x78 0x9c 开头。
func zlibCompress(plain []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(plain); err != nil {
		return nil, fmt.Errorf("%w: zlib 压缩: %v", ErrParam, err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("%w: zlib 压缩收尾: %v", ErrParam, err)
	}
	return buf.Bytes(), nil
}

// maybeInflate 仅在数据带 zlib 头时解压，否则原样返回。
func maybeInflate(data []byte) ([]byte, error) {
	if !hasZlibHeader(data) {
		return data, nil
	}
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: 构造 zlib 解压器: %v", ErrDecrypt, err)
	}
	defer r.Close()

	out, err := io.ReadAll(io.LimitReader(r, maxPlainBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: zlib 解压: %v", ErrDecrypt, err)
	}
	return out, nil
}

// maxPlainBytes 限制解压后的大小，防止压缩炸弹。
const maxPlainBytes = 32 << 20

// hasZlibHeader 按 zlib 魔数判断：首字节 0x78，次字节的低 4 位为 8（deflate），且满足校验位约束。
func hasZlibHeader(data []byte) bool {
	if len(data) < 2 || data[0] != 0x78 {
		return false
	}
	cmf, flg := int(data[0]), int(data[1])
	return (cmf<<8+flg)%31 == 0
}

// aesCBCEncrypt 做 AES-CBC 加密并补 PKCS7 填充。
func aesCBCEncrypt(plain, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: 构造 AES: %v", ErrParam, err)
	}
	padded := pkcs7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}

// aesCBCDecrypt 解密并严格校验 PKCS7 填充。
//
// 填充校验失败是「协议参数版本不匹配」最可靠的信号：换错 key 时解出的明文几乎不可能满足填充约束。
func aesCBCDecrypt(cipherText, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: 构造 AES: %v", ErrParam, err)
	}
	bs := block.BlockSize()
	if len(cipherText) == 0 || len(cipherText)%bs != 0 {
		return nil, fmt.Errorf("%w: 密文长度 %d 不是 %d 的整数倍", ErrDecrypt, len(cipherText), bs)
	}
	plain := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, cipherText)
	return pkcs7Unpad(plain, bs)
}

// pkcs7Pad 追加 PKCS7 填充；空输入也会补满一整块。
func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// pkcs7Unpad 校验并去除 PKCS7 填充。
func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	n := len(data)
	if n == 0 || n%blockSize != 0 {
		return nil, fmt.Errorf("%w: 填充前长度 %d 非法", ErrDecrypt, n)
	}
	pad := int(data[n-1])
	if pad == 0 || pad > blockSize || pad > n {
		return nil, fmt.Errorf("%w: 填充字节 %d 非法", ErrDecrypt, pad)
	}
	for i := n - pad; i < n; i++ {
		if data[i] != byte(pad) {
			return nil, fmt.Errorf("%w: 填充内容不一致", ErrDecrypt)
		}
	}
	return data[:n-pad], nil
}
