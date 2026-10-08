package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// caseTimeout bounds one compat case; some create and delete several tables.
const caseTimeout = 10 * time.Minute

func (h *Handler) compatRoutes(r *gin.Engine) {
	r.GET("/compat/cases", h.compatCases)
	r.POST("/compat/run", h.compatRun)
	r.POST("/compat/cleanup", h.compatCleanup)
}

func (h *Handler) compatCases(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"cases": h.compat.Cases(), "coverage": h.compat.Coverage()})
}

// compatRun runs one case (?id=) for a test run (?run=, which namespaces the
// tables it creates) and answers 200 with the result whether it passed or not.
func (h *Handler) compatRun(c *gin.Context) {
	run, ok := queryRun(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), caseTimeout)
	defer cancel()
	res, found := h.compat.Run(ctx, c.Query("id"), run)
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown case", "code": "not_found"})
		return
	}
	c.JSON(http.StatusOK, res)
}

// compatCleanup deletes the tables a run created through this cell.
func (h *Handler) compatCleanup(c *gin.Context) {
	run, ok := queryRun(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), caseTimeout)
	defer cancel()
	deleted, err := h.compat.Cleanup(ctx, run)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": deleted})
}
