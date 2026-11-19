module github.com/pixelplanet/connect-power-control

go 1.26.11

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/prusa3d/prusa-connect/sdk v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.44.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
)

replace github.com/prusa3d/prusa-connect/sdk => ./.core/sdk
