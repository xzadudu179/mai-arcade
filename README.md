# mai-arcade

通过玩家二维码（SGWCMAID）从舞萌 DX 机台读取完整成绩并同步到查分器的工具。提供 CLI 与本地服务两种形态。实现了机台协议，实现完整成绩接口`GetUserMusicApi`，并能够拿到 FC / FS 标识。

适用于需要把机台原始成绩（含竞速标识）搬进查分器的人；以及要把这套能力接进自己服务的开发者（零依赖 Go 单二进制，命令行与 HTTP / WebSocket 两种接入方式）。

---

## 注意事项

- 一. **协议版本会随游戏更新失效，且失效症状与"IP 被阻断"一致。**

   服务端对不再接受的协议版本返回的是 `HTTP 200` + 0 字节 Response，与出口 IP 被阻断时的现象在传输层一致：

   | Mai-Encoding       | 结果                           |
   | ------------------ | ------------------------------ |
   | 1.55               | `32B 密文 → {"result":"Pong"}` |
   | 1.53 / 1.52 / 1.50 | `HTTP 200` + 0 字节            |

   短时间内请求过多会触发临时封禁，效果同样是 0 字节。请求过多也会出现封禁。建议同一账号 5 分钟不超过 3 次，不应在循环里重试。

   所以出现 0 字节问题时应先更换版本，等待一阵子再尝试更换网络。`probe` 会逐版本尝试，并告诉你应该用哪个，例如：

   ```
   title -> version_stale   accepted=1.55
   detail: 配置的版本 1.53 拿不到可用响应，而版本 1.55 正常；1.53=0 字节，1.55=正常
   hint:   协议版本已更新：加 --version 1.55，或用 --auto-version 自动选用；
           这不是出口 IP 问题，换网络不会改善
   ```

   另一种"空白内容"是 16 字节密文：`HTTP 200` + 16 字节，能解密但内容是空的（`789c030000000001` 为空 zlib。说明请求已经到达业务逻辑但被拒绝，可能是以下几种成因：

   1. 会话被占用：`GetUserPreviewApi` 返回的 `isLogin: true` 为此状态。上一个会话没正常登出会保留到 15~20 分钟超时，期间取成绩与登出都只回空体（小黑屋）
   2. 请求过快：`GetGameSettingApi` 返回的 `gameSetting.requestInterval` 要求两次请求间隔 ≥ 1200 毫秒。连发请求会被丢弃，产生该问题。

   区分方法：先检查 `isLogin`，若为 true 就等会话释放，否则确认请求间隔是否达标。本项目已在 `TitleClient.waitTurn` 里按 `DefaultRequestInterval` 主动节流。
   3. 未回传会话 Cookie：服务端在 `GetUserPreviewApi` 等响应里用 `Set-Cookie` 下发 `JSESSIONID`，后续每个请求都必须带上它，否则一律返回空体。`transport.Response` 本就保留响应头，`TitleClient.absorbSession` 会记下它并在后续请求回传。

   正常交互完整链路：

   ```
   [AimeDB]      errorID=0 userId=10807675
   [1. GameSetting] 200
   [2. Preview]     200 isLogin=false，响应头带 Set-Cookie: JSESSIONID=...
   [3. 登录]        200 returnCode=1
   [3.5 UserData]  200
   [4. 取成绩]      200 1008 条，字段含 comboStatus / syncStatus
   [5. 登出]        200 returnCode=1（会话正常释放）
   ```

   取成绩返回的字段：

   ```json
    {
        "musicId": 17,
        "level": 3,
        "playCount": 1,
        "achievement": 986301,
        "comboStatus": 0,
        "syncStatus": 0,
        "deluxscoreMax": 721,
        "scoreRank": 9
    }
   ```

- 二. 出口 IP 可能被封：若**所有**协议版本都返回 0 字节，说明大概率是 IP 问题，可以尝试换家宽 IP / 重启光猫换 IP / 等 48–72 小时。云服务器 IP 风险最高。

- 三. 本项目没有官方授权渠道：协议来自社区公开实现。只操作用户**自己扫码授权**的账号；不会批量扫号、伪造成绩、写入他人数据等损害账号的操作。

- 四. 账号与 IP 都有被限制的风险：二维码是 1.53+ 的账号唯一凭证；同账号并发登录会互相挤掉；请求频率过高会触发封禁。因此本项目：同账号操作串行、用完登出、**不要随意重试**。

---

## 快速开始

需要 Go 1.22+（验证环境 Go 1.25.5）。无第三方依赖，`go.mod` 只有一行 module 声明。

```bash
# 1. 构建（产出 CLI 与服务两个单二进制，可直接 scp 到别的机器）
make build

# 2. 连通性自检：确认这台机器能跟机台通信
./bin/mai-arcade probe; echo "退出码=$?"
```

`probe` 的判定与退出码：

| class           | 含义             | 处理                                              | 退出码 |
| --------------- | ---------------- | ------------------------------------------------- | ------ |
| `ok`            | 正常             | —                                                 | 0      |
| `version_stale` | 版本不被接受     | 可按照提示更换 `--version`，或用 `--auto-version` | 3      |
| `params`        | 无法解析响应体   | 换 `--version`                                    | 3      |
| `blocked`       | 出口 IP 被阻断   | 更换 IP / 等待 48–72h                             | 2      |
| `network`       | TCP/TLS 建连失败 | 检查代理、防火墙、DNS                             | 2      |
| `business`      | 收到业务错误码   | 按错误码表处理                                    | 1      |

```bash
# 3. 用二维码拉成绩（二维码在 舞萌|中二 服务号 → 玩家二维码）
echo "$SGID" | ./bin/mai-arcade verify
```

`SGID` 就是登录二维码的内容，形如 `SGWCMAID` + 12 位时间戳 + 64 位十六进制，共 84 字符。有效期 10 分钟，过期要重新获取。图片链接也接受，程序会自动提取并补回 `SGWC` 前缀。

> 二维码建议通过 stdin，不要写在命令行参数里防止暴露。

---

## 命令参考

统一命令约定：

- **退出码表达结果**：`0` 成功 / `1` 业务失败 / `2` 网络或阻断 / `3` 参数错误
- **结构化结果为 stdout**（JSON），**日志为 stderr**。
- stdout 恒为一行 JSON，形如 `{"ok":true,"command":"...","data":{...}}` 或 `{"ok":false,"command":"...","error":{...}}`

通用参数（每个子命令都支持）：

| 参数                                                 | 说明                                                        |
| ---------------------------------------------------- | ----------------------------------------------------------- |
| `--version`                                          | 协议版本，默认 `1.55`，可选 `1.40 1.50 1.51 1.52 1.53 1.55` |
| `--auto-version`                                     | 依次尝试 `1.55` / `1.53`，使用探测到的可用版本              |
| `--proxy`                                            | HTTP 代理地址                                               |
| `--timeout`                                          | 单次请求超时，默认 `30s`                                    |
| `--session-timeout`                                  | 单次会话总时长上限，默认 `10m`，必须小于机台 15 分钟超时    |
| `--chart-cache-dir`                                  | 曲目数据缓存目录（约 1 MB，按天有效）                       |
| `--chart-cache-ttl`                                  | 曲目缓存有效期，默认 `24h`                                  |
| `--no-reuse`                                         | 关闭同一二维码在有效期内的结果复用                          |
| `--log-level`                                        | `debug` / `info` / `warn` / `error`，默认 `info`            |
| `--title-base-url`、`--aime-url`、`--chart-base-url` | 调试用，覆盖默认地址                                        |

### `probe` 检测连通性

```bash
./bin/mai-arcade probe
./bin/mai-arcade probe --auto-version  # 自动检测版本随后判定
```

该操作不消耗二维码，不建立会话。会探测 AimeDB：AimeDB 侧使用合成二维码，只覆盖 chipID、时间戳与 commonKey，若服务端返回"二维码不可用"则证明请求格式与签名算法已被接受。

### `verify` 只读拉取成绩、打印字段结构

```bash
echo "$SGID" | ./bin/mai-arcade verify
```

示例数据：

```json
{"ok":true,"command":"verify","data":{
 "userId":10807675,"scoreCount":1438,
 "withComboStatus":1203,"withSyncStatus":988,"utageCount":62,
 "comboStatusCounts":{"ap":312,"app":44,"fc":601,"fcp":78,"none":235},
 "syncStatusCounts":{"fs":494,"fsd":120,"fsdp":18,"fsp":156,"none":450,"sync":200},
 "utages":[{"musicId":100508,"title":"[協]恋愛裁判","type":"DX","levelIndex":0,"rawLevel":5,
            "achievement":100.5,"dxScore":2100,"combo":"fc","sync":"fs"}],
 "samples":[{"musicId":8,"level":"Master","achievement":101.0,"dxScore":2711,"combo":"ap","sync":"sync","playCount":12}]}}
```

输出里 `withSyncStatus` 不为 0 就证明机台返回的是完整成绩接口（包含了 FC / FS 等信息）。

`utages` 是宴谱的映射结果：`title` 非空说明这个 `musicId` 能在查分器曲目库里找到、同步时会带着 `levelIndex=0` 上传；`title` 为空（同时 `utageUnmapped` 计数增加）说明索引里没有它，同步时会被当作未知曲目跳过。

### `sync` 拉取成绩并同步到水鱼

```bash
echo "$SGID" | ./bin/mai-arcade sync --fish-token "$FISH_TOKEN"
# 或统一写法
echo "$SGID" | ./bin/mai-arcade sync --site divingfish --credential "$FISH_TOKEN"
```

Import-Token 通过登录 [水鱼查分器](https://www.diving-fish.com/maimaidxprober/) → "编辑个人资料"→ 生成 Import-Token。

同步请先读取现状再合并：水鱼对 `fc`/`fs` 的取值有白名单（`fc/fcp/ap/app`、`sync/fs/fsp/fsd/fsdp`），**不在白名单里的值会被服务端置空**。而机台数据在部分情况下拿不到 FC/FS，直接上传就会把服务器上已有的标记清空。所以流程固定为"读取当前值 → 合并（机台缺的字段保留原值）→ 上传"。

其他细节：宴谱（`musicId >= 100000`）**一并上传**，但机台把它报成 `level=5`、查分器里每首宴谱只有一档，所以上传前折算成 `level_index=0`（见 `model.Score.ChartLevel`）；宴谱不参与定数，因此不计入 b50。曲目索引里无法查找的曲目跳过；同名曲以上传的 `title` 精确匹配（如 `Link` 与 `Link(CoF)` 属于两首曲目）。

折算依据来自水鱼公开的测试数据（`GET /player/test_data`，无需鉴权）：里面有 4 条宴谱成绩，`level_index` 全是 `0`、`ra` 全是 `0`（说明水鱼自己也不给宴谱算 rating），而这四条 `song_id`（`100508` / `100327` / `111355` / `111359`）在水鱼曲目表里都能找到同名的宴谱条目——**机台 musicId 与水鱼宴谱 id 是同一套编号**。

### `profile` 账号资料

```bash
echo "$SGID" | ./bin/mai-arcade profile
```

> **待验证**：`GetUserDataApi` / `GetUserPreviewApi` 的字段名没有真实响应可校对。解析目前为宽松解析：无字段为 0 值。

### `b50` 计算 b50 与 rating

```bash
echo "$SGID" | ./bin/mai-arcade b50
```

定数（`ds`）与"新曲 / 旧曲"划分来自查分器曲目库——机台成绩本身不带定数。

---

## 服务模式（`mai-arcade-server`）

把同一组能力暴露成本地服务，供 bot、脚本或其他进程调用。**操作定义与安全策略在两个接入方式之间共享**，只有传输方式的区别。

```bash
export MAI_TOKEN=$(head -c 32 /dev/urandom | base64)
./bin/mai-arcade-server --token-env MAI_TOKEN                # 只读，监听 127.0.0.1:8787
./bin/mai-arcade-server --token-env MAI_TOKEN --allow-write  # 允许 sync 写入查分器
```

### 安全默认值

| 默认                   | 原因                                                     |
| ---------------------- | -------------------------------------------------------- |
| 监听 `127.0.0.1:8787`  | 本服务持有账号凭证与查分器写权限，默认不对外             |
| **写操作关闭**         | 只读为安全默认值，`sync` 必须显式 `--allow-write` 才可用 |
| 必须提供令牌           | 不能无鉴权运行；需要至少 16 字符长度的令牌。             |
| 全局限频 6 次 / 2 分钟 | 防止不当操作导致出口 IP 被封禁                           |
| 请求体上限 64 KB       | 本服务的请求只含少量参数                                 |

令牌来源：`--token-env NAME`（推荐）、`--token-file PATH`（权限过宽会警告）、`--token`（会进 `ps`，仅本机调试）。

### HTTP 接入

```bash
# 存活探针（不鉴权）
curl -s localhost:8787/healthz

# 查看可用操作与全部错误码（自描述）
curl -s localhost:8787/v1/ops -H "Authorization: Bearer $MAI_TOKEN"

# 执行操作：POST /v1/op/{操作名}，请求体是 JSON 参数
curl -s -X POST localhost:8787/v1/op/probe -d '{}' \
     -H "Authorization: Bearer $MAI_TOKEN"

curl -s -X POST localhost:8787/v1/op/b50 -d '{"sgid":"SGWCMAID..."}' \
     -H "Authorization: Bearer $MAI_TOKEN"
```

操作一律走 `POST` + 请求体：二维码与凭证放进 query 会被访问日志记下来。鉴权头支持 `Authorization: Bearer <token>` 与等价的 `X-Auth-Token: <token>`。

### WebSocket 接入

浏览器无法给 WebSocket 自定义请求头，因此除 `Authorization` 外还支持子协议通道：`Sec-WebSocket-Protocol: mai-arcade.v1.bearer.<token>`（命中时服务端回显 `mai-arcade.v1`）。

连接建立后服务端先发一条 hello 说明版本与可用操作，之后客户端发送：

```json
{"id":"1","op":"verify","args":{"sgid":"SGWCMAID..."}}
```

服务端回 `{"type":"result","id":"1","ok":true,"data":{...}}` 或 `{"type":"error","id":"1","ok":false,"error":{...}}`。一条连接上的请求按到达顺序串行处理（机台侧本就要求同账号串行）。

### 与 CLI 的关系

|          | CLI                  | 服务                           |
| -------- | -------------------- | ------------------------------ |
| 结果     | stdout JSON + 退出码 | HTTP 状态码 + JSON 信封        |
| 错误标识 | `error.code`         | `error.code` + `error.status`  |
| 凭证     | 每次调用传参         | 服务端持有，调用方只需服务令牌 |
| 适合     | 一次性脚本、cron     | bot、常驻进程、多客户端        |

双方共用同一套错误码（见下）。

---

## 错误码表

`error.code` 是对外稳定的标识，调用方应按它分支；`kind` 只有三档，决定退出码。完整目录也可从运行中的服务读取：`GET /v1/ops` 的 `errorCodes` 字段，或 WebSocket 的 hello 消息。

| code                                | kind               | 含义                                   |
| ----------------------------------- | ------------------ | -------------------------------------- |
| `sgid_format`                       | param              | 二维码格式非法                         |
| `sgid_expired`                      | param              | 二维码已过期                           |
| `empty_response`                    | network            | 标题服务器返回 0 字节响应（TLS 正常）  |
| `decrypt`                           | param              | 响应解密失败                           |
| `version_mismatch`                  | param              | 响应解密成功但不是合法 JSON            |
| `version_unsupported`               | param              | 不支持的协议版本                       |
| `config_invalid`                    | param              | 配置不合法                             |
| `already_logged_in`                 | business           | 账号已在登录状态                       |
| `timeout`                           | network            | 请求超时                               |
| `aime_unavailable`                  | network            | AimeDB 无有效响应                      |
| `aime_qr_rejected`                  | param              | AimeDB 拒绝该二维码（已过期或不存在）  |
| `aime_bad_signature`                | param              | AimeDB 拒绝请求签名                    |
| `site_credential_invalid`           | business           | 水鱼 Import-Token 无效                 |
| `site_unexpected_status`            | business           | 查分器返回非预期状态（读取阶段）       |
| `site_upload_failed`                | business           | 查分器拒绝接收成绩（上传阶段）         |
| `site_chart_index_unavailable`      | param              | 缺少曲目索引，无法确定歌名             |
| `site_scores_unmappable`            | business           | 机台成绩无法映射到任何曲目，已放弃上传 |
| `site_records_unparsable`           | business           | 查分器成绩响应无法解析                 |
| `unknown_site`                      | param              | 未知查分器                             |
| `usage` / `param`                   | param              | 参数用法 / 参数错误                    |
| `network` / `business` / `internal` | network / business | 分类兜底码                             |
| `unauthorized`                      | param              | 鉴权失败                               |
| `rate_limited`                      | business           | 请求过于频繁，已限频                   |
| `payload_too_large`                 | param              | 请求体过大                             |
| `write_disabled`                    | param              | 写操作未启用                           |
| `unknown_operation`                 | param              | 未知操作                               |
| `bad_arguments`                     | param              | 参数不合法                             |
| `busy`                              | business           | 服务繁忙，并发已满                     |

HTTP 状态映射是**逐个码显式决定**的（`internal/server/server.go` 的 `codeStatus`），不靠分类兜底——有一条测试强制每个新码都补上这一行。

---

## rating 与 b50 的算法依据

**公式**（Gen 3 / Splash PLUS 至今）：

```
单曲 ra = floor(定数 × 评级系数 × min(达成率, 100.5) / 100)
总 rating = 新曲 ra 最高的 15 条 + 旧曲 ra 最高的 35 条 之和   （宴谱不计入）
```

评级系数表：`100.5%+ → 22.4`，之后每档递减 `21.6 / 21.1 / 20.8 / 20.3 / 20.0 / 16.8 / 15.2 / 13.6 / 12.0 / 11.2 / 9.6 / 8.0 / 6.4 / 4.8 / 3.2 / 1.6 / 0`。

`testdata/rating_vectors.json` 取自水鱼公开的 `GET /api/maimaidxprober/player/test_data`（官方提供的测试数据）。里面每条成绩均携带 `ra`，以及服务端给出的 `rating` 总和。

- 31 条 `ra` 向量逐条复现（覆盖 13 个系数档位）
- 用本实现的 `ra` 重算 b50，`rating` 与服务器值相等

---

## 故障排查

排查顺序：先 `probe`，再 `verify --log-level debug`。日志里能看到每次机台调用的 api 名、hash、字节数（**不含**二维码与 token 全文）。

| 现象                                              | 判定                                               | 怎么办                                                                                        |
| ------------------------------------------------- | -------------------------------------------------- | --------------------------------------------------------------------------------------------- |
| `class=version_stale`                             | 配置的协议版本已不被接受                           | 根据提示换 `--version`，或用 `--auto-version`。                                               |
| `class=blocked`（所有版本都 0 字节）              | 三种成因：版本不被接受 / 触发封禁 / 出口 IP 被阻断 | 按顺序排除：换 `--version` 并等待 15~20 分钟、换家宽 IP / 重启光猫 / 等 48–72 小时            |
| `UserLoginApi` 返回 **HTTP 500**（Tomcat 错误页） | 登录请求的字段形状与版本不符                       | 检查 `internal/protocol/version.go` 里该版本的 `Login` 形状（字段名、时间单位、`isContinue`） |
| `class=params`                                    | 无法解析响应                                       | `--version` 换一个版本                                                                        |
| `class=network`                                   | TCP/TLS 建连失败                                   | 查 `--proxy`、防火墙、DNS 能否解析 `maimai-gm.wahlap.com:42081`                               |
| `class=business`，或 `returnCode=100`             | 账号已在登录状态                                   | 会话残留。等约 15 分钟自动释放                                                                |
| `returnCode=102`                                  | 二维码过期                                         | 重新从公众号获取（有效期约 10 分钟）                                                          |
| `returnCode=110`                                  | KeyChip 不匹配                                     | 机台参数问题，检查 chipID 与协议版本                                                          |
| AimeDB `errorID=1` / `2`                          | 二维码过期或不存在                                 | 重新获取二维码                                                                                |
| AimeDB `errorID=50`                               | 签名错误                                           | 检查本机时区：时间戳按**本地时区**生成。                                                      |
| "二维码格式非法"                                  | 输入不完整                                         | 确认 84 字符、`SGWCMAID` 开头、后 64 位是十六进制；图片链接也可以                             |
| 水鱼 `导入token有误`                              | Import-Token 无效                                  | 去水鱼官网"编辑个人资料"重新生成                                                              |
| "登出未成功，账号会话可能在约 15 分钟内不可用"    | 登出请求失败                                       | 等会话超时；期间该账号登不上机台。检查网络稳定性                                              |
| 服务返回 401                                      | 服务令牌不对                                       | 确认客户端与服务端用的是同一个 `--token`                                                      |
| 服务返回 403 `write_disabled`                     | 写操作未开启                                       | 服务端增加参数 `--allow-write`                                                                |
| 服务返回 429                                      | 触发限频                                           | 按 `Retry-After` 等待，限频保护出口 IP                                                        |
| 服务返回 503 `busy`                               | 并发占满                                           | 稍后重试，同账号的机台会话本就串行                                                            |
| `verify` 条数为 0                                 | 账号没有游玩记录，或接口结构变了                   | 换账号确认，若确信用过机台，检查是否机台号与手台号不同。                                      |

---

## 架构概览

```
cmd/mai-arcade ─┐
                ├─→ service ─┬─→ sync ─┐
cmd/mai-arcade- │            │          ├─→ transport
server          │            └─ protocol ┘
      │         │                  ↓
      └─→ server ─→ ops ────────── model   （叶子：共享数据模型、错误分类、错误码目录）
                      │           chart   （叶子：曲目索引与本地缓存）
                      └─→ access    （叶子：鉴权、限频、限额）
                     server ─→ wsproto（WebSocket 实现）
              rating  （叶子：ra 与 b50 计算，只依赖 model）
```

| 包                      | 职责                                                          |
| ----------------------- | ------------------------------------------------------------- |
| `cmd/mai-arcade`        | CLI 入口：解析参数、装配依赖、分发子命令                      |
| `cmd/mai-arcade-server` | 服务入口：装配接入层、进程生命周期、优雅关闭                  |
| `internal/service`      | 编排"扫码 → 登录 → 取分 → 登出 → 同步"；版本探测、结果复用    |
| `internal/protocol`     | 机台协议：二维码解析、AimeDB 换账号、标题服务器加解密与调用   |
| `internal/sync`         | 查分器侧：`Syncer` 接口 + 注册表 + 水鱼实现（含合并）         |
| `internal/chart`        | `musicId ↔ 歌名/类型/定数/新旧曲` 索引，含本地缓存            |
| `internal/rating`       | 单曲 ra 与 b50 计算                                           |
| `internal/transport`    | HTTP 细节：超时、代理、响应上限、日志脱敏                     |
| `internal/ops`          | **与接入方式无关的操作层**：服务能做什么、写保护、参数校验    |
| `internal/server`       | 接入层：HTTP 与 WebSocket 两个适配器，共用鉴权/限频/错误映射  |
| `internal/wsproto`      | 零依赖的 WebSocket（RFC 6455）服务端：握手 + 帧编解码         |
| `internal/access`       | 请求准入：Bearer 鉴权、令牌桶限频、体积与并发限额             |
| `internal/model`        | 叶子包：成绩与枚举、错误分类（`Kind` 对应退出码）、错误码目录 |

<!-- ### 为什么自己实现 WebSocket

Go 标准库没有 WebSocket，而本项目的核心决策之一是无第三方依赖（单静态二进制、可随时 scp 到别的网络环境、供应链干净）。服务端只需要握手与帧两件事，实现量可控，可用 RFC 自带的官方样例断言；测试里另有一个**独立实现的客户端**与服务端互操作，而不是共用同一份可能出错的编解码。 -->

### 接入方式与操作分开

`ops` 定义"能做什么"（`{id, op, args}` → 结果或错误码），`server` 负责传输消息与结果。因此：

- 新增一个操作：只改 `ops`，HTTP 与 WebSocket 同时获得该能力
- 新增一种接入方式：只加一个 `internal/server/<name>.go`，操作与安全策略不需要变动
- 鉴权、限频、限额、错误码映射只有一份实现

---

## 常见任务索引（我要做 X → 改哪个文件）

| 我要做                              | 改哪里                                                                                                                                                                                                                        |
| ----------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **加一个协议版本**（游戏更新后）    | `internal/protocol/version.go` 的 `Versions` 表加一项，若自动探测也要认它，同步更新 `internal/service/version.go` 的 `versionProbeOrder` 与 `protocol.DefaultVersion`；再往 `internal/protocol/crypto_test.go` 补 hash 对照值 |
| **确认当前该用哪个版本**            | `make integration` 运行 `TestIntegrationDefaultProtocolVersionIsAccepted`，会逐版本测试并报告                                                                                                                                 |
| **登录被服务端拒绝（HTTP 500）**    | 对照 `internal/protocol/version.go` 的 `Version.Login`（字段名 / 时间单位 / `isContinue` / 登出 `type`）与社区参考实现的登录体逐项核对；改完补充 `internal/protocol/title_test.go` 的按版本字段断言                             |
| **加一个查分器**                    | 新增 `internal/sync/<名字>.go`，实现 `Name()` 与 `Upload()`，在 `init()` 里 `Register("名字", func(d Deps) Syncer { ... })` 一行。既有文件无需改动。                   |
| **加一个机台 API**                  | `internal/protocol/api.go` 加 API 名常量与请求/响应结构体（字段名照抄官方，**含 `acsessCode` 这类官方拼写错误**）；在 `title.go` 加薄方法，内部复用 `call()`；补一条表驱动测试                                                |
| **加一个操作**（服务与 CLI 都能用） | 在 `internal/ops/operations.go` 写一个 `Run` 函数，在 `init()` 里 `Register(Operation{...})` 一行；需要写权限就设 `Mutating: true`                                                                                            |
| **加一个 CLI 子命令**               | 新增 `cmd/mai-arcade/<名字>.go`，写 `run<Name>(ctx, *environment, args) error`；在 `main.go` 的 `commands` map 加一行                                                                                                         |
| **加一种接入方式**（如 gRPC）       | 新增 `internal/server/<名字>.go`，把请求解成 `ops.Dispatch(ctx, deps, op, args)`；鉴权用 `access.Authenticator`，限频用 `access.RateLimiter`                                                                                  |
| **加一个错误码**                    | 在 `internal/model/codes.go` 加常量并列入 `DeclaredCodes()`，在拥有该错误的包里加哨兵并 `model.RegisterCode(...)`，最后在 `internal/server/server.go` 的 `codeStatus` 补状态映射（测试会强制你补）                            |
| **换协议版本默认值**                | `internal/protocol/version.go` 的 `DefaultVersion`                                                                                                                                                                            |
| **调超时 / 会话时长 / 限频**        | CLI：`--timeout` / `--session-timeout`；服务：`--op-timeout` / `--rate-burst` / `--rate-interval`（默认值在 `cmd/mai-arcade-server/config.go`）                                                                               |
| **改同步合并策略**                  | `internal/sync/merge.go` 的 `merge()`，配套改 `internal/sync/merge_test.go`                                                                                                                                                   |
| **改 rating 系数表**                | `internal/rating/b50.go` 的 `coefficientTable`，跑 `internal/rating` 的真值测试确认                                                                                                                                           |
| **改错误码 → HTTP 状态映射**        | `internal/server/server.go` 的 `codeStatus`                                                                                                                                                                                   |

---

## 安全须知

- **二维码（SGWCMAID）是账号唯一凭证**（1.53+）。有效期内持有者即可登录该账号。本项目不会以任何形式包括落盘、写日志显示二维码，回显 `String()` 与JSON 序列化都只输出"前缀 + 长度"，因此即使误塞进日志或结构化输出也不会泄露。
- **token 同理**（AimeDB token、水鱼 Import-Token、服务令牌）。日志里只出现前 4 位，或只说明来源。
- 二维码**只经 stdin 传入**（CLI），或放在 POST 请求体里（服务）。避免进入访问日志。
- 结果复用缓存以二维码的哈希为键，不保留原文。
- 服务默认**只读**且只监听回环地址；`--allow-write` 会改动真实查分器数据，启用前请提前想好。
- 日志走 stderr，结构化结果走 stdout。
- **不要把二维码、token、真实成绩贴进 issue、聊天群或日志附件。**
- 本项目只操作用户自己扫码授权的账号。

---

## 开发

```bash
make test           # 全部单测，不访问网络
make test-race      # 带竞态检测（并发代码必过）
make test-isolated  # 在无网络的命名空间里跑单测，验证"单测不访问真实接口"
make lint           # gofmt 检查 + go vet（含 integration 标签下的文件）
make build          # 构建 bin/mai-arcade 与 bin/mai-arcade-server
make release        # 交叉编译 linux/amd64 与 linux/arm64
make integration    # 需要真实网络的集成测试；需要凭证的用例会自动跳过
```

测试规模：约 7.2k 行实现 + 7.9k 行测试，265 个测试函数 / 565 个用例。

`make test-isolated` 不是摆设：它靠 `unshare -rn` 切断网络后重跑全部单测（`-count=1` 跳过缓存）。写这套测试时正是它抓出了真实缺陷——`sync` 原本在校验 `--site` 之前就去拉曲目索引，于是拼错的站点名会被报成网络故障而不是参数错误。

**目录约定**：`internal/` 保证外部无法 import；文件超过约 300 行或出现第二个职责拆分。

### 集成测试与手动验证

需要真实凭证的用例受 `//go:build integration` 隔离，默认不运行：

```bash
# 只读验证：连通性、当前可用协议版本、曲目库结构（不写入任何数据）
make integration

# 需要二维码的完整链路验证（二维码约 10 分钟有效）
MAI_SGID='SGWCMAID...' make integration

# 需要水鱼凭证的合并验证（只读，不上传）
MAI_FISH_TOKEN='...' make integration
```

**同步验证（会写入真实账号，必须由人明确发起）**：建议先在一个测试账号上确认"`fc` 传空是否会被清空"这一前提是否仍然成立：

1. 在测试账号上保留至少一条带 FC 标记的成绩
2. 运行 `sync`，然后回查分器网页确认那条 FC 还在
3. 若被清空，说明合并策略失效，请检查 `internal/sync/merge.go`

这最后一步刻意**没有**自动化——它会改变真实账号的数据。

---

## 许可与声明

**风险声明**：本项目使用社区逆向得到的协议，**无官方授权渠道**。使用可能导致**账号被限制、出口 IP 被封禁（最长 48–72 小时）**。请自行评估风险。

**使用边界**：只操作用户自己扫码授权的账号。禁止批量扫号、禁止成绩伪造、禁止写入他人数据、禁止用于商业爬取。不主动获取二维码（生成二维码需要注入微信客户端，属于另一个项目）；不实现高风险写接口（改门、发票券、写收藏品）。
