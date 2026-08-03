package zenalog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Howthemeaning/isaac0914-utils/zenserver/ginx"
)

// GinHandler 提供 GET /activityLog 的处理函数，解析 query 参数走 ginx 响应壳。
// engine.GET("/activityLog", logger.GinHandler()) 一行接入。
//
// 参数：mode（flat 默认 / trace）、startTime/endTime（RFC3339）、pageNum、
// pageSize、query、instanceId、condition（可重复，格式 field:op:value，
// op 取 eq/ne/prefix）。参数错误回 BAD_REQUEST，查询失败回 INTERNAL_ERROR。
func (l *Logger) GinHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		req, err := parseListRequest(c)
		if err != nil {
			ginx.BadRequest(c, err.Error())
			return
		}
		// 用 request context：requestId/操作人挂在它上面，用 c 当 context 日志就断链
		res, err := l.List(c.Request.Context(), req)
		if err != nil {
			if errors.Is(err, errInvalidRequest) {
				ginx.BadRequest(c, err.Error())
				return
			}
			ginx.InternalError(c, err)
			return
		}
		ginx.Success(c, res)
	}
}

// parseListRequest query 参数 → ListRequest，解析失败必须报错，不静默降级
func parseListRequest(c *gin.Context) (ListRequest, error) {
	var req ListRequest
	var zero ListRequest

	switch mode := c.Query("mode"); mode {
	case "", "flat":
		req.Mode = ModeFlat
	case "trace":
		req.Mode = ModeByTrace
	default:
		return zero, fmt.Errorf("unknown mode %q, want flat or trace", mode)
	}

	var err error
	if req.PageNum, err = intQuery(c, "pageNum"); err != nil {
		return zero, err
	}
	if req.PageSize, err = intQuery(c, "pageSize"); err != nil {
		return zero, err
	}
	if req.StartTime, err = timeQuery(c, "startTime"); err != nil {
		return zero, err
	}
	if req.EndTime, err = timeQuery(c, "endTime"); err != nil {
		return zero, err
	}
	req.Query = c.Query("query")
	req.InstanceID = c.Query("instanceId")

	for _, raw := range c.QueryArray("condition") {
		cond, err := parseCondition(raw)
		if err != nil {
			return zero, err
		}
		req.Conditions = append(req.Conditions, cond)
	}
	return req, nil
}

func intQuery(c *gin.Context, name string) (int, error) {
	raw := c.Query(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("param %s=%q is not an integer", name, raw)
	}
	return n, nil
}

func timeQuery(c *gin.Context, name string) (time.Time, error) {
	raw := c.Query(name)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("param %s=%q is not RFC3339", name, raw)
	}
	return t, nil
}

// parseCondition "field:op:value" → Condition，value 里允许再出现冒号
func parseCondition(raw string) (Condition, error) {
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		return Condition{}, fmt.Errorf("condition %q malformed, want field:op:value", raw)
	}
	var op Op
	switch parts[1] {
	case "eq":
		op = OpEq
	case "ne":
		op = OpNe
	case "prefix":
		op = OpPrefix
	default:
		return Condition{}, fmt.Errorf("condition %q has unknown op %q, want eq/ne/prefix", raw, parts[1])
	}
	return Condition{Field: parts[0], Op: op, Value: parts[2]}, nil
}
