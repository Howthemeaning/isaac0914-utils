// Package ginx 提供 gin 的内置中间件和统一响应壳。
//
// 响应壳沿用 success/ret/code/msg/data 格式：HTTP 状态码一律 200，
// 业务结果看 success 与 code。
package ginx

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 成功响应码
const (
	CodeSuccess  = "SUCCESS"  // 操作成功
	CodeCreated  = "CREATED"  // 资源创建成功
	CodeAccepted = "ACCEPTED" // 请求已接受（异步任务）
)

// 客户端错误码
const (
	CodeBadRequest   = "BAD_REQUEST"   // 请求参数错误
	CodeUnauthorized = "UNAUTHORIZED"  // 未授权
	CodeForbidden    = "FORBIDDEN"     // 禁止访问
	CodeNotFound     = "NOT_FOUND"     // 资源不存在
	CodeInvalidParam = "INVALID_PARAM" // 参数验证失败
	CodeConflict     = "CONFLICT"      // 资源冲突
)

// 服务端错误码
const (
	CodeInternalError      = "INTERNAL_ERROR"         // 服务器内部错误
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"    // 服务不可用
	CodeDBError            = "DATABASE_ERROR"         // 数据库错误
	CodeExternalError      = "EXTERNAL_SERVICE_ERROR" // 外部服务错误
)

// Response 统一响应结构
type Response struct {
	Success bool   `json:"success"`
	Ret     int    `json:"ret"`
	Code    string `json:"code"`
	Msg     string `json:"msg"`
	Data    any    `json:"data,omitempty"`
}

// Success 成功响应
func Success(c *gin.Context, data any) {
	writeOK(c, CodeSuccess, "success", data)
}

// SuccessWithMsg 带消息的成功响应
func SuccessWithMsg(c *gin.Context, msg string, data any) {
	writeOK(c, CodeSuccess, msg, data)
}

// Created 资源创建成功
func Created(c *gin.Context, data any) {
	writeOK(c, CodeCreated, "success", data)
}

// Accepted 请求已接受，异步处理中
func Accepted(c *gin.Context, data any) {
	writeOK(c, CodeAccepted, "success", data)
}

// Fail 失败响应
func Fail(c *gin.Context, code, msg string) {
	writeFail(c, code, msg)
}

// Error 失败响应，消息取自 err
func Error(c *gin.Context, code string, err error) {
	writeFail(c, code, errMsg(err))
}

// BadRequest 请求参数错误
func BadRequest(c *gin.Context, msg string) {
	writeFail(c, CodeBadRequest, msg)
}

// NotFound 资源不存在
func NotFound(c *gin.Context, msg string) {
	writeFail(c, CodeNotFound, msg)
}

// InternalError 服务器内部错误
func InternalError(c *gin.Context, err error) {
	writeFail(c, CodeInternalError, errMsg(err))
}

func writeOK(c *gin.Context, code, msg string, data any) {
	c.JSON(http.StatusOK, Response{Success: true, Ret: 0, Code: code, Msg: msg, Data: data})
}

func writeFail(c *gin.Context, code, msg string) {
	c.JSON(http.StatusOK, failResponse(code, msg))
}

func failResponse(code, msg string) Response {
	return Response{Success: false, Ret: -1, Code: code, Msg: msg}
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
