package v1

import (
	"fmt"
	"net"
)

func joinIPPort(ip string, port int) string {
	return net.JoinHostPort(ip, fmt.Sprint("%d", port))
}
