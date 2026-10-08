package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/curioso/extenddb-yugabytedb/app/internal/store"
)

type Handler struct {
	cellID string
	users  *store.UserStore
}

func NewRouter(cellID string, users *store.UserStore) *gin.Engine {
	h := &Handler{cellID: cellID, users: users}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), h.cellHeader)

	r.GET("/health", h.health)
	r.POST("/users", h.putUser)
	r.GET("/users/:id", h.getUser)
	return r
}

// cellHeader tags every response with the cell that served it.
func (h *Handler) cellHeader(c *gin.Context) {
	c.Header("X-Cell-Id", h.cellID)
	c.Next()
}

// health reports healthy only when this cell's ExtendDB answers.
func (h *Handler) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()

	if err := h.users.Ping(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unhealthy", "cell": h.cellID, "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "healthy", "cell": h.cellID})
}

type putUserRequest struct {
	ID    string `json:"id" binding:"required"`
	Name  string `json:"name" binding:"required"`
	Email string `json:"email" binding:"required,email"`
}

func (h *Handler) putUser(c *gin.Context) {
	var req putUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u := store.User{
		ID:        req.ID,
		Name:      req.Name,
		Email:     req.Email,
		Cell:      h.cellID,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.users.Put(c.Request.Context(), u); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, u)
}

func (h *Handler) getUser(c *gin.Context) {
	u, err := h.users.Get(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, u)
}
