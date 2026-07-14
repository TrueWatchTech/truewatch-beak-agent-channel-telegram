module github.com/TrueWatch/beak-agent-channel-telegram/conformance

go 1.23

replace github.com/TrueWatch/beak-agent-channel-telegram => ../

replace gitlab.jiagouyun.com/guance/beak-agent-channel-sdk/beak-channel-sdk-conformance => github.com/GuanceCloud/beak-channel-sdk-conformance v0.0.27

require (
	github.com/TrueWatch/beak-agent-channel-telegram v0.0.0-00010101000000-000000000000
	gitlab.jiagouyun.com/guance/beak-agent-channel-sdk/beak-channel-sdk-conformance v0.0.0-00010101000000-000000000000
)
