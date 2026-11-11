module github.com/coldcoder126/rein/chat/api/openai

go 1.26.0

require (
	github.com/coldcoder126/rein v0.0.0
	github.com/coldcoder126/rein/chat/imageproc v0.0.0
	github.com/openai/openai-go/v3 v3.71.1
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.1.2 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/invopop/jsonschema v0.14.0 // indirect
	github.com/pb33f/ordered-map/v2 v2.3.1 // indirect
	github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd // indirect
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	go.yaml.in/yaml/v4 v4.0.0-rc.2 // indirect
	golang.org/x/image v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/coldcoder126/rein => ../../..

replace github.com/coldcoder126/rein/chat/imageproc => ../../../chat/imageproc
