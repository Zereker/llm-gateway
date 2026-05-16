package admin

import (
	"errors"

	"github.com/gin-gonic/gin"
)

// registerSubscriptionRoutes 注册订阅 CRUD（挂在 accounts 子路径下）：
//
//	GET    /accounts/:pin/subscriptions               列 account 已订阅模型
//	POST   /accounts/:pin/subscriptions               订阅一个模型 {"model_service_id": 1}
//	PUT    /accounts/:pin/subscriptions/:msid         切 enabled {"enabled":false}
//	DELETE /accounts/:pin/subscriptions/:msid         软删订阅
//
// 不暴露 subscription 自己的 BIGINT id；admin 都按 (account_pin, model_service_id) 复合操作。
func registerSubscriptionRoutes(api *gin.RouterGroup, s *SubscriptionStore) {
	api.GET("/accounts/:pin/subscriptions", listSubscriptions(s))
	api.POST("/accounts/:pin/subscriptions", subscribe(s))
	api.PUT("/accounts/:pin/subscriptions/:msid", setSubscriptionEnabled(s))
	api.DELETE("/accounts/:pin/subscriptions/:msid", unsubscribe(s))
}

func listSubscriptions(s *SubscriptionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		pin := c.Param("pin")
		all, err := s.ListByAccount(c.Request.Context(), pin)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		items := make([]subscriptionDTO, len(all))
		for i := range all {
			items[i] = subscriptionToDTO(&all[i])
		}
		c.JSON(200, gin.H{"items": items})
	}
}

func subscribe(s *SubscriptionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		pin := c.Param("pin")
		var req subscribeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.ModelServiceID == 0 {
			c.JSON(400, gin.H{"error": "model_service_id required"})
			return
		}
		row, err := s.Subscribe(c.Request.Context(), pin, req.ModelServiceID)
		if err != nil {
			if errors.Is(err, ErrAlreadySubscribed) {
				c.JSON(409, gin.H{"error": err.Error()})
				return
			}
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, subscriptionToDTO(row))
	}
}

func setSubscriptionEnabled(s *SubscriptionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		pin := c.Param("pin")
		msID, err := parseInt64Param(c, "msid")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		var req subscriptionToggleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.Enabled == nil {
			c.JSON(400, gin.H{"error": "enabled required"})
			return
		}
		if err := s.SetEnabled(c.Request.Context(), pin, msID, *req.Enabled); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

func unsubscribe(s *SubscriptionStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		pin := c.Param("pin")
		msID, err := parseInt64Param(c, "msid")
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if err := s.Unsubscribe(c.Request.Context(), pin, msID); err != nil {
			c.JSON(404, gin.H{"error": err.Error()})
			return
		}
		c.Status(204)
	}
}

type subscribeRequest struct {
	ModelServiceID int64 `json:"model_service_id"`
}

type subscriptionToggleRequest struct {
	Enabled *bool `json:"enabled"`
}
