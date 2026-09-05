// 私有扩展（不属于 upstream sub2api）
//
// 所含内容：setPrivateEnvReachableDefaults —— 为 fork 私有配置项注册零值默认，
// 使它们可以通过环境变量注入。
//
// 背景：viper.Unmarshal 只解码 AllKeys() 返回的 key，而 AllKeys() 是
// SetDefault / 配置文件 / 显式 BindEnv 三者的并集。AutomaticEnv 只能覆盖已在
// 并集里的 key，永远不会新增 key（viper_bind_struct 逃生口在 -tags embed 下被
// 编译掉）。所以没有注册默认值的私有配置项，在没有 config.yaml 的纯环境变量
// 部署里是不可达的：运维设了变量，loader 静默丢弃。
// upstream 的 TestConfigKeysAreEnvReachable 会守护这一点。
//
// 这里的值一律为零值：key 缺失时本来就 unmarshal 成零值，注册零值保持行为完全
// 一致，只是让 key 变得可从环境变量寻址。需要更丰富默认值的子系统在
// unmarshal 之后自行应用（例如 AccountModelQualityCache 的
// defaultQualityWindowMinutes），与注册侧无关。
//
// merge 策略：本文件为纯新增，不与 upstream 冲突。config.go 里仅有一行 hook
// 调用（setEnvReachableDefaults 末尾），upstream 若改动该函数只需保留这一行。
package config

import "github.com/spf13/viper"

// setPrivateEnvReachableDefaults 注册 fork 私有配置项的零值默认。
//
// 覆盖范围：
//   - gateway.request_log.*          请求内容记录
//
// 新增私有配置项时必须同步在此登记，否则 TestConfigKeysAreEnvReachable 会失败。
func setPrivateEnvReachableDefaults() {
	// gateway.request_log：请求内容记录
	viper.SetDefault("gateway.request_log.enabled", false)
	viper.SetDefault("gateway.request_log.max_body_bytes", 0)
}
