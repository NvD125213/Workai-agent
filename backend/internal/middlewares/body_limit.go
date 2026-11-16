package middlewares

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyLimit caps the request body; reads beyond n bytes fail during decoding.
func BodyLimit(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}
