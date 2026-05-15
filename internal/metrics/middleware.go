// Package metrics provides Prometheus instrumentation for the Launch service.
package metrics

import (
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

// IPRestrictionMiddleware returns a Gin handler that restricts access
// to the listed IP addresses and CIDR ranges. An empty list permits
// all clients (suitable for development).
func IPRestrictionMiddleware(allowedIPs []string) gin.HandlerFunc {
	if len(allowedIPs) == 0 {
		return func(c *gin.Context) { c.Next() }
	}

	var nets []*net.IPNet
	var ips []net.IP

	for _, entry := range allowedIPs {
		_, network, err := net.ParseCIDR(entry)
		if err == nil {
			nets = append(nets, network)
			continue
		}
		if ip := net.ParseIP(entry); ip != nil {
			ips = append(ips, ip)
		}
	}

	return func(c *gin.Context) {
		clientIP := net.ParseIP(c.ClientIP())
		if clientIP == nil {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}

		for _, network := range nets {
			if network.Contains(clientIP) {
				c.Next()
				return
			}
		}
		for _, ip := range ips {
			if ip.Equal(clientIP) {
				c.Next()
				return
			}
		}

		c.AbortWithStatus(http.StatusForbidden)
	}
}
