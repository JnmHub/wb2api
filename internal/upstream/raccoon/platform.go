package raccoon

import "runtime"

// clientPlatform reports the X-Client-Platform value of the running host.
func clientPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "desktop-windows"
	case "darwin":
		return "desktop-macos"
	}
	return "desktop-linux"
}
