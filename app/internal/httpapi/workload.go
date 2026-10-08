package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/curioso/extenddb-yugabytedb/app/internal/ddbx"
	"github.com/curioso/extenddb-yugabytedb/app/internal/workload"
)

// auditTimeout bounds the audit endpoints, which scan tables and streams.
const auditTimeout = 2 * time.Minute

// The workload endpoints report a write's outcome through the status class:
//
//	2xx  acknowledged: the write was applied
//	4xx  rejected: the write was not applied (409 for a failed condition)
//	5xx  unknown: ExtendDB failed or timed out, the write may have been applied
func (h *Handler) workloadRoutes(r *gin.Engine) {
	r.POST("/registers/:id", h.registerWrite)
	r.POST("/registers/:id/cas", h.registerCAS)
	r.GET("/registers/:id", h.registerGet)

	r.POST("/claims/:id", h.claim)
	r.GET("/claims/:id", h.claimGet)

	r.POST("/counters/:id/incr", h.counterIncr)
	r.GET("/counters/:id", h.counterGet)

	r.POST("/accounts/seed", h.accountsSeed)
	r.POST("/accounts/transfer", h.transfer)
	r.GET("/accounts/snapshot", h.accountsSnapshot)
	r.GET("/accounts/audit", h.accountsAudit)
	r.POST("/accounts/ledger", h.ledgerPresent)

	r.POST("/orders", h.orderPut)
	r.POST("/orders/batch", h.ordersBatchPut)
	r.POST("/orders/get", h.ordersGet)
	r.POST("/orders/status", h.orderSetStatus)
	r.POST("/orders/delete", h.orderDelete)
	r.GET("/orders/customer/:customer", h.ordersByCustomer)
	r.GET("/orders/status/:run/:status", h.ordersByStatus)
	r.GET("/orders/audit", h.ordersAudit)
	r.GET("/orders/stream-audit", h.ordersStreamAudit)
}

func (h *Handler) opCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), h.opTimeout)
}

func auditCtx(c *gin.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), auditTimeout)
}

// fail maps a store error to the status classes described on workloadRoutes.
func fail(c *gin.Context, err error) {
	status, code := http.StatusBadGateway, ddbx.Code(err)
	switch {
	case errors.Is(err, workload.ErrConflict):
		status, code = http.StatusConflict, "condition_failed"
	case errors.Is(err, workload.ErrInsufficientFunds):
		status, code = http.StatusConflict, "insufficient_funds"
	case errors.Is(err, workload.ErrTransactionConflict):
		status, code = http.StatusConflict, "transaction_conflict"
	case ddbx.Rejected(err):
		status = http.StatusUnprocessableEntity
	case ddbx.TimedOut(err):
		status, code = http.StatusGatewayTimeout, "timeout"
	}
	c.JSON(status, gin.H{"error": err.Error(), "code": code})
}

func bind(c *gin.Context, req any) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "code": "bad_request"})
		return false
	}
	return true
}

// queryInt reads a required positive integer query parameter.
func queryInt(c *gin.Context, name string) (int64, bool) {
	v, err := strconv.ParseInt(c.Query(name), 10, 64)
	if err != nil || v <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": name + " must be a positive integer", "code": "bad_request"})
		return 0, false
	}
	return v, true
}

func queryRun(c *gin.Context) (string, bool) {
	run := c.Query("run")
	if run == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "run is required", "code": "bad_request"})
	}
	return run, run != ""
}

func (h *Handler) registerWrite(c *gin.Context) {
	var req struct {
		Value string `json:"value" binding:"required"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	reg, err := h.work.RegisterWrite(ctx, c.Param("id"), req.Value)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, reg)
}

func (h *Handler) registerCAS(c *gin.Context) {
	var req struct {
		Value    string `json:"value" binding:"required"`
		Expected int64  `json:"expected_version" binding:"gte=0"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	reg, err := h.work.RegisterCAS(ctx, c.Param("id"), req.Expected, req.Value)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, reg)
}

func (h *Handler) registerGet(c *gin.Context) {
	ctx, cancel := h.opCtx(c)
	defer cancel()
	reg, found, err := h.work.RegisterGet(ctx, c.Param("id"), c.Query("consistent") != "false")
	switch {
	case err != nil:
		fail(c, err)
	case !found:
		c.JSON(http.StatusNotFound, gin.H{"error": "register not found", "code": "not_found"})
	default:
		c.JSON(http.StatusOK, reg)
	}
}

func (h *Handler) claim(c *gin.Context) {
	var req struct {
		Owner string `json:"owner" binding:"required"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	if err := h.work.Claim(ctx, c.Param("id"), req.Owner); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": c.Param("id"), "owner": req.Owner})
}

func (h *Handler) claimGet(c *gin.Context) {
	ctx, cancel := h.opCtx(c)
	defer cancel()
	owner, err := h.work.ClaimOwner(ctx, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "owner": owner})
}

func (h *Handler) counterIncr(c *gin.Context) {
	var req struct {
		Op string `json:"op" binding:"required"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	value, err := h.work.CounterIncr(ctx, c.Param("id"), req.Op)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": c.Param("id"), "value": value})
}

func (h *Handler) counterGet(c *gin.Context) {
	ctx, cancel := h.opCtx(c)
	defer cancel()
	counter, err := h.work.CounterGet(ctx, c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, counter)
}

func (h *Handler) accountsSeed(c *gin.Context) {
	var req struct {
		Run     string `json:"run" binding:"required"`
		Count   int    `json:"count" binding:"required,gt=0,lte=100"`
		Balance int64  `json:"balance" binding:"required,gt=0"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := auditCtx(c)
	defer cancel()
	if err := h.work.AccountsSeed(ctx, req.Run, req.Count, req.Balance); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, req)
}

func (h *Handler) transfer(c *gin.Context) {
	var req workload.Transfer
	if !bind(c, &req) {
		return
	}
	if *req.From == *req.To {
		c.JSON(http.StatusBadRequest, gin.H{"error": "from and to must differ", "code": "bad_request"})
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	duplicate, err := h.work.Transfer(ctx, req)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": req.Token, "duplicate": duplicate})
}

func (h *Handler) accountsSnapshot(c *gin.Context) {
	run, ok := queryRun(c)
	count, ok2 := queryInt(c, "count")
	if !ok || !ok2 {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	balances, err := h.work.AccountsSnapshot(ctx, run, int(count))
	if err != nil {
		fail(c, err)
		return
	}
	var sum int64
	for _, b := range balances {
		sum += b
	}
	c.JSON(http.StatusOK, gin.H{"balances": balances, "sum": sum})
}

func (h *Handler) accountsAudit(c *gin.Context) {
	run, ok := queryRun(c)
	count, ok2 := queryInt(c, "count")
	balance, ok3 := queryInt(c, "balance")
	if !ok || !ok2 || !ok3 {
		return
	}
	ctx, cancel := auditCtx(c)
	defer cancel()
	audit, err := h.work.AccountsAudit(ctx, run, int(count), balance)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, audit)
}

func (h *Handler) ledgerPresent(c *gin.Context) {
	var req struct {
		Run    string   `json:"run" binding:"required"`
		Tokens []string `json:"tokens" binding:"required"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := auditCtx(c)
	defer cancel()
	present, err := h.work.LedgerPresent(ctx, req.Run, req.Tokens)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"present": present})
}

func (h *Handler) orderPut(c *gin.Context) {
	var req workload.Order
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	if err := h.work.OrderPut(ctx, req); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, workload.OrderKey{CustomerID: req.CustomerID, OrderID: req.OrderID})
}

func (h *Handler) ordersBatchPut(c *gin.Context) {
	var req struct {
		Orders []workload.Order `json:"orders" binding:"required,min=1,max=25,dive"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	if err := h.work.OrdersBatchPut(ctx, req.Orders); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"written": len(req.Orders)})
}

func (h *Handler) ordersGet(c *gin.Context) {
	var req struct {
		Keys []workload.OrderKey `json:"keys" binding:"required,dive"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := auditCtx(c)
	defer cancel()
	orders, err := h.work.OrdersGet(ctx, req.Keys)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

func (h *Handler) orderSetStatus(c *gin.Context) {
	var req struct {
		workload.OrderKey
		Run    string `json:"run" binding:"required"`
		Status string `json:"status" binding:"required"`
	}
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	order, err := h.work.OrderSetStatus(ctx, req.OrderKey, req.Run, req.Status)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, order)
}

func (h *Handler) orderDelete(c *gin.Context) {
	var req workload.OrderKey
	if !bind(c, &req) {
		return
	}
	ctx, cancel := h.opCtx(c)
	defer cancel()
	if err := h.work.OrderDelete(ctx, req); err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, req)
}

func (h *Handler) ordersByCustomer(c *gin.Context) {
	ctx, cancel := h.opCtx(c)
	defer cancel()
	orders, err := h.work.OrdersByCustomer(ctx, c.Param("customer"), c.Query("index") == "lsi", c.Query("consistent") != "false")
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

func (h *Handler) ordersByStatus(c *gin.Context) {
	ctx, cancel := h.opCtx(c)
	defer cancel()
	orders, err := h.work.OrdersByStatus(ctx, c.Param("run"), c.Param("status"))
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

func (h *Handler) ordersAudit(c *gin.Context) {
	run, ok := queryRun(c)
	if !ok {
		return
	}
	ctx, cancel := auditCtx(c)
	defer cancel()
	audit, err := h.work.OrdersAudit(ctx, run)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, audit)
}

func (h *Handler) ordersStreamAudit(c *gin.Context) {
	run, ok := queryRun(c)
	if !ok {
		return
	}
	ctx, cancel := auditCtx(c)
	defer cancel()
	audit, err := h.work.OrdersStreamAudit(ctx, run)
	if err != nil {
		fail(c, err)
		return
	}
	c.JSON(http.StatusOK, audit)
}
