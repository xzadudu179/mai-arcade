# mai-arcade

用玩家二维码（SGWCMAID）从舞萌 DX 机台读取**完整成绩（含 FC / FS 竞速标识）**，并同步到查分器。提供 CLI 与本地服务两种形态。

写它的原因：查分器公开 API 没有二维码入口，只能读库里已有成绩；而现成封装库走的是**对手成绩接口**，源码里直接写死 `fc=None, fs=None`，拿不到竞速标识。本项目自己实现机台协议，走官方完整成绩接口（`GetUserMusicApi`）。

**给谁用**：需要把机台原始成绩（含竞速标识）搬进查分器的人；以及要把这套能力接进自己服务的开发者（零依赖 Go 单二进制，命令行与 HTTP / WebSocket 两种接入方式）。

---

## 状态与限制

| 项 | 状态 |
| --- | --- |
| 协议版本 | 支持 1.40 / 1.50 / 1.51 / 1.52 / 1.53 / 1.55，**默认 1.55**（实测可用版本） |
| `api_hash` 算法 | **[已验证]** 通过文档给出的 1.50 三条官方向量（单测断言） |
| 机台连通与协议参数 | **[已验证]** 1.55 实测能建连、解密并拿到 `{"result":"Pong"}`；`probe` 逐个版本探活 |
| AimeDB（二维码换账号） | **[已验证]** 实测连通；`probe` 用合成二维码验证签名被服务端接受 |
| 标题服务器（Ping / 加解密 / 路径） | **[已验证]** 1.55 实测建连并解出 `{"result":"Pong"}` |
| 登录 / 取分 / 登出 | **[已验证]** 真实二维码实测全链路成功：`1008 条成绩 / 1 轮 / 正常登出`，其中 comboStatus(FC/AP) 290 条、syncStatus(FS/SYNC) 622 条。踩过的三个坑依次是 `clientId`/`placeId` 传空（HTTP 500）、请求间隔不足（空体）、**未回传 `JSESSIONID`**（空体） |
| 登录字段形状 | **[已验证]** 1.55 实测 `returnCode: 1`；字段为 `accessCode` / 秒级 `dateTime` / `loginDateTime` / `isContinue:false` / `regionId:8` / 登出带 `type` |
| 水鱼同步（含 FC/FS 合并） | **[已验证]** 真实账号端到端跑通：机台 1008 条上传成功，同步前后对比 FC 277→282、FS 618→624（只增不减），证明机台缺标记时水鱼原值未被清空 |
| b50 / rating 公式 | **[已验证]** 用真实数据逐条断言，见「rating 与 b50 的算法依据」 |
| 本地服务（HTTP + WebSocket） | **[已验证]** 鉴权、限频、写保护、错误码映射、WS 握手与帧均有测试；服务已实跑 |
| 账号资料字段名 | **[已验证]** `GetUserDataApi` / `GetUserPreviewApi` 的真实响应已校对（含 `playerRating`、`frameId`、`plateId` 等） |

### 已知限制（务必先读）

1. **协议版本会随游戏更新失效，且失效症状与「IP 被阻断」一模一样**

   服务端对**不再接受的协议版本**返回的是 `HTTP 200` + **0 字节响应体**，与出口 IP 被阻断时的现象在传输层完全同形。实测对照（同一 IP、同一路径，只改 `Mai-Encoding`）：

   | Mai-Encoding | 结果 |
   | --- | --- |
   | 1.55 | `32B 密文 → 解密成功：{"result":"Pong"}` |
   | 1.53 / 1.52 / 1.50 | `HTTP 200` + 0 字节 |

   **第三种成因是封禁**：短时间内请求过多会触发临时封禁，症状同样是 0 字节。本项目在排查阶段连续打了约 30 个请求（版本矩阵 + 登录变体矩阵），随后 1.55 也从「正常」变成「0 字节」——这就是封禁。**排查时务必克制**：同一账号 5 分钟不超过 3 次，绝不要在循环里重试。

   所以**看到 0 字节先换版本，再等十几分钟，最后才考虑换网络**。`probe` 会逐个版本试，并直接告诉你应该用哪个：

   ```
   title -> version_stale   accepted=1.55
   detail: 配置的版本 1.53 拿不到可用响应，而版本 1.55 正常；1.53=0 字节，1.55=正常
   hint:   协议版本已更新：加 --version 1.55，或用 --auto-version 自动选用；
           这不是出口 IP 问题，换网络不会改善
   ```

   （原始规范把上述现象判定为「出口 IP 被华立阻断」。社区确实有 IP 被封的记录，但那不是本例的成因。）

   **另一种「空」是 16 字节密文**：`HTTP 200` + 16 字节，能解密但内容是空的（`789c030000000001` 是空 zlib 流）。这与 0 字节**不是一回事**——它说明请求已经到达业务逻辑但被拒绝，实测两种成因：

   - **会话被占用**：`GetUserPreviewApi` 返回的 `isLogin: true` 就是此状态。上一个会话没正常登出会保留到 15 分钟硬超时，期间取成绩与登出都只回空体（本项目首次 verify 即如此：登录成功、登出失败，之后一切返回空体）
   - **请求过频**：`GetGameSettingApi` 返回的 `gameSetting.requestInterval` 要求两次请求间隔 ≥ **1200 毫秒**（实测值）。连发请求会被丢弃，症状同样是空体

   区分方法：先看 `isLogin`，为 true 就等会话释放；否则确认请求间隔是否达标。本项目已在 `TitleClient.waitTurn` 里按 `DefaultRequestInterval` 主动节流。

   **第三种成因（最容易漏）：没回传会话 Cookie**。服务端在 `GetUserPreviewApi` 等响应里用 `Set-Cookie` 下发 `JSESSIONID`，**后续每个请求都必须带上它**，否则一律返回空体。`transport.Response` 本就保留响应头，`TitleClient.absorbSession` 会记下它并在后续请求回传——缺了这一步，症状与上两种完全一样，排查时极难想到。

   **实测打通后的完整链路**（每步都与服务端正常交互）：

   ```
   [AimeDB]      errorID=0 userId=10807675
   [① GameSetting] 200 ✅
   [② Preview]     200 ✅ isLogin=false，响应头带 Set-Cookie: JSESSIONID=...
   [③ 登录]        200 ✅ returnCode=1
   [③.5 UserData]  200 ✅
   [④ 取成绩]      200 ✅ 1008 条，字段含 comboStatus / syncStatus
   [⑤ 登出]        200 ✅ returnCode=1（会话正常释放）
   ```

   取成绩返回的字段（就是本项目存在的意义）：

   ```json
   {"musicId": 17, "level": 3, "playCount": 1, "achievement": 986301,
    "comboStatus": 0, "syncStatus": 0, "deluxscoreMax": 721, "scoreRank": 9}
   ```

2. **出口 IP 仍可能被真封**：若**所有**协议版本都返回 0 字节，才是 IP 问题——换家宽 IP / 重启光猫换 IP / 等 48–72 小时。云服务器 IP 风险最高。

3. **本项目没有官方授权渠道**：协议来自社区公开实现。只操作用户**自己扫码授权**的账号；不做批量扫号、不做成绩伪造、不写入他人数据。

4. **账号与 IP 都有被限制的风险**：二维码是 1.53+ 的账号唯一凭证；同账号并发登录会互相挤掉；请求频率过高会触发封禁。因此本项目：同账号操作串行、用完必登出、**不做无脑重试**。

5. **登录已跑通（1.55）**：实测 `UserLoginApi` 返回 `returnCode: 1`，会话建立成功。最初拿到 `HTTP 500`（Tomcat 错误页）的根因是**登录与登出请求的 `clientId`、`placeId` 传了空值**——服务端建会话时要拿它们匹配机台，空值会让它抛未捕获异常。两者现已作为数据放进版本参数表（`DefaultClientID` 取 keychip 去掉横线后的前 11 位，`DefaultPlaceID` 取门店号），不再有硬编码空串。

   复验步骤：`probe`（应 `title: class=ok`）→ `echo "$SGID" | mai-arcade verify`（应打出成绩条数与 `withSyncStatus`）。若仍 500，用 `--log-level debug` 看请求字节数与 api hash，并与 `internal/protocol/api.go` 的字段形状逐项对照。

---

## 快速开始

需要 Go 1.22+（验证环境 Go 1.25.5）。**无任何第三方依赖**，`go.mod` 只有一行 module 声明。

```bash
# 1. 构建（产出 CLI 与服务两个单二进制，可直接 scp 到别的机器）
make build

# 2. 连通性自检：确认这台机器能跟机台通信
./bin/mai-arcade probe; echo "退出码=$?"
```

`probe` 的判定与退出码：

| class | 含义 | 处理 | 退出码 |
| --- | --- | --- | --- |
| `ok` | 正常 | — | 0 |
| `version_stale` | 链路通，但配置的版本已不被接受 | 按提示换 `--version`，或用 `--auto-version` | 3 |
| `params` | 收到的响应解不开 | 换 `--version` | 3 |
| `blocked` | 所有版本都 0 字节 | **出口 IP 被阻断**，换 IP / 等 48–72h | 2 |
| `network` | TCP/TLS 建连失败 | 检查代理、防火墙、DNS | 2 |
| `business` | 收到业务错误码 | 按错误码表处理 | 1 |

```bash
# 3. 用二维码拉成绩（二维码从哪来：舞萌 DX 公众号 → 我的 → 二维码）
echo "$SGID" | ./bin/mai-arcade verify
```

`SGID` 就是公众号里那串/那张二维码的内容，形如 `SGWCMAID` + 12 位时间戳 + 64 位十六进制，共 84 字符。**有效期约 10 分钟**，过期要重新获取。图片链接也接受，程序会自动提取并补回 `SGWC` 前缀。

> **二维码只走 stdin，不要写在命令行参数里**——`ps` 会暴露 argv。

---

## 命令参考

所有命令的约定：

- **退出码表达结果**：`0` 成功 / `1` 业务失败 / `2` 网络或阻断 / `3` 参数错
- **结构化结果走 stdout**（JSON），**日志走 stderr**。调用方只需解析 JSON，不用解析人类可读文本
- stdout 恒为一行 JSON，形如 `{"ok":true,"command":"...","data":{...}}` 或 `{"ok":false,"command":"...","error":{...}}`

通用参数（每个子命令都支持）：

| 参数 | 说明 |
| --- | --- |
| `--version` | 协议版本，默认 `1.55`，可选 `1.40 1.50 1.51 1.52 1.53 1.55` |
| `--auto-version` | 依次尝试 1.55 / 1.53，用探测到的可用版本 |
| `--proxy` | HTTP 代理地址（注意：代理出口同样可能被拦） |
| `--timeout` | 单次请求超时，默认 `30s` |
| `--session-timeout` | 单次会话总时长上限，默认 `10m`，必须小于机台 15 分钟硬超时 |
| `--chart-cache-dir` | 曲目数据缓存目录（约 1 MB，按天有效） |
| `--chart-cache-ttl` | 曲目缓存有效期，默认 `24h` |
| `--no-reuse` | 关闭同一二维码在有效期内的结果复用 |
| `--log-level` | `debug` / `info` / `warn` / `error`，默认 `info` |
| `--title-base-url`、`--aime-url`、`--chart-base-url` | 调试用，覆盖默认地址 |

### `probe` —— 连通性自检（无需凭证）

```bash
./bin/mai-arcade probe                 # 用配置的版本判定
./bin/mai-arcade probe --auto-version  # 先探出版本，再判定
```

不消耗二维码、不建立会话。除了机台，也会探测 AimeDB（换账号是链路第一步）：AimeDB 那一侧用**合成二维码**——签名只覆盖 chipID、时间戳与 commonKey，与二维码内容无关，因此服务端只要回「二维码不可用」就证明请求格式与签名算法都被接受了。

### `verify` —— 拉成绩、打印字段结构（不上传）

```bash
echo "$SGID" | ./bin/mai-arcade verify
```

```json
{"ok":true,"command":"verify","data":{
 "userId":10807675,"scoreCount":1438,
 "withComboStatus":1203,"withSyncStatus":988,"utageCount":62,
 "comboStatusCounts":{"ap":312,"app":44,"fc":601,"fcp":78,"none":235},
 "syncStatusCounts":{"fs":494,"fsd":120,"fsdp":18,"fsp":156,"none":450,"sync":200},
 "samples":[{"musicId":8,"level":"Master","achievement":101.0,"dxScore":2711,"combo":"ap","sync":"sync","playCount":12}]}}
```

（数值为示例形状；真实条数取决于账号。）

**这条命令的验收意义**：输出里 `withSyncStatus` 不为 0，就证明机台返回的是**完整成绩接口**、确实带着 FC/FS——这正是本项目存在的理由。

### `sync` —— 拉成绩并同步到查分器

```bash
echo "$SGID" | ./bin/mai-arcade sync --fish-token "$FISH_TOKEN"
# 或统一写法
echo "$SGID" | ./bin/mai-arcade sync --site divingfish --credential "$FISH_TOKEN"
```

Import-Token 从哪来：登录 [水鱼查分器](https://www.diving-fish.com/maimaidxprober/) → 「编辑个人资料」→ 生成 Import-Token。

**同步一定先读现状再合并**：水鱼对 `fc`/`fs` 的取值有白名单（`fc/fcp/ap/app`、`sync/fs/fsp/fsd/fsdp`），**不在白名单里的值会被服务端置空**。而机台数据在部分情况下拿不到 FC/FS，直接上传就会把服务器上已有的标记清空。所以流程固定为「读现状 → 合并（机台缺的字段保留原值）→ 上传」。

其他细节：宴谱（`musicId >= 100000`）跳过；曲目索引里查不到的曲目跳过；同名曲以上传的 `title` 精确匹配（如 `Link` 与 `Link(CoF)` 是两首）。

### `profile` —— 账号资料

```bash
echo "$SGID" | ./bin/mai-arcade profile
```

> **待验证**：`GetUserDataApi` / `GetUserPreviewApi` 的字段名没有真实响应可校对。解析刻意做得宽松——缺字段取零值，不会让整条命令失败。

### `b50` —— 计算 b50 与 rating

```bash
echo "$SGID" | ./bin/mai-arcade b50
```

定数（`ds`）与「新曲 / 旧曲」划分来自查分器曲目库——机台成绩本身不带定数。

---

## 服务模式（`mai-arcade-server`）

把同一组能力暴露成本地服务，供 bot、脚本或其他进程调用。**操作定义与安全策略在两个接入方式之间完全共享**，只是传输不同。

```bash
export MAI_TOKEN=$(head -c 32 /dev/urandom | base64)
./bin/mai-arcade-server --token-env MAI_TOKEN                # 只读，监听 127.0.0.1:8787
./bin/mai-arcade-server --token-env MAI_TOKEN --allow-write  # 允许 sync 写入查分器
```

### 安全默认值

| 默认 | 原因 |
| --- | --- |
| 监听 `127.0.0.1:8787` | 本服务持有账号凭证与查分器写权限，默认不对外 |
| **写操作关闭** | 只读应当是不用想就能选的安全默认值；`sync` 必须显式 `--allow-write` 才可用 |
| 必须提供令牌 | 不能无鉴权运行；令牌至少 16 字符，比较用常量时间实现 |
| 全局限频 6 次 / 2 分钟 | 真正稀缺的是出口 IP，多客户端共享同一条线路 |
| 请求体上限 64 KB | 本服务的请求只含少量参数 |

令牌来源三选一：`--token-env NAME`（推荐）、`--token-file PATH`（权限过宽会警告）、`--token`（会进 `ps`，仅本机调试）。

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

连接建立后服务端先发一条 hello 说明版本与可用操作，之后客户端发：

```json
{"id":"1","op":"verify","args":{"sgid":"SGWCMAID..."}}
```

服务端回 `{"type":"result","id":"1","ok":true,"data":{...}}` 或 `{"type":"error","id":"1","ok":false,"error":{...}}`。一条连接上的请求按到达顺序串行处理（机台侧本就要求同账号串行）。

### 与 CLI 的关系

| | CLI | 服务 |
| --- | --- | --- |
| 结果 | stdout JSON + 退出码 | HTTP 状态码 + JSON 信封 |
| 错误标识 | `error.code` | `error.code` + `error.status` |
| 凭证 | 每次调用传参 | 服务端持有，调用方只需服务令牌 |
| 适合 | 一次性脚本、cron | bot、常驻进程、多客户端 |

两边的**错误码是同一套**（见下）。

---

## 错误码表

`error.code` 是对外稳定的标识，调用方应按它分支；`kind` 只有三档，决定退出码。完整目录也可从运行中的服务读取：`GET /v1/ops` 的 `errorCodes` 字段，或 WebSocket 的 hello 消息。

| code | kind | 含义 |
| --- | --- | --- |
| `sgid_format` | param | 二维码格式非法 |
| `sgid_expired` | param | 二维码已过期 |
| `empty_response` | network | 标题服务器返回 0 字节响应（TLS 正常） |
| `decrypt` | param | 响应解密失败 |
| `version_mismatch` | param | 响应解密成功但不是合法 JSON |
| `version_unsupported` | param | 不支持的协议版本 |
| `config_invalid` | param | 配置不合法 |
| `already_logged_in` | business | 账号已在登录状态 |
| `timeout` | network | 请求超时 |
| `aime_unavailable` | network | AimeDB 无有效响应 |
| `aime_qr_rejected` | param | AimeDB 拒绝该二维码（已过期或不存在） |
| `aime_bad_signature` | param | AimeDB 拒绝请求签名 |
| `site_credential_invalid` | business | 水鱼 Import-Token 无效 |
| `site_unexpected_status` | business | 查分器返回非预期状态（读取阶段） |
| `site_upload_failed` | business | 查分器拒绝接收成绩（上传阶段） |
| `site_chart_index_unavailable` | param | 缺少曲目索引，无法确定歌名 |
| `site_scores_unmappable` | business | 机台成绩无法映射到任何曲目，已放弃上传 |
| `site_records_unparsable` | business | 查分器成绩响应无法解析 |
| `unknown_site` | param | 未知查分器 |
| `usage` / `param` | param | 参数用法 / 参数错误 |
| `network` / `business` / `internal` | network / business | 分类兜底码 |
| `unauthorized` | param | 鉴权失败 |
| `rate_limited` | business | 请求过于频繁，已限频 |
| `payload_too_large` | param | 请求体过大 |
| `write_disabled` | param | 写操作未启用 |
| `unknown_operation` | param | 未知操作 |
| `bad_arguments` | param | 参数不合法 |
| `busy` | business | 服务繁忙，并发已满 |

HTTP 状态映射是**逐个码显式决定**的（`internal/server/server.go` 的 `codeStatus`），不靠分类兜底——有一条测试强制每个新码都补上这一行。

---

## rating 与 b50 的算法依据

> 这部分与原始规范的出入最大：规范把 b50 标为「算法待验证」，实现时用真实数据把它钉死了。

**公式**（Gen 3 / Splash PLUS 至今）：

```
单曲 ra = floor(定数 × 评级系数 × min(达成率, 100.5) / 100)
总 rating = 新曲 ra 最高的 15 条 + 旧曲 ra 最高的 35 条 之和   （宴谱不计入）
```

评级系数表：`100.5%+ → 22.4`，之后每档递减 `21.6 / 21.1 / 20.8 / 20.3 / 20.0 / 16.8 / 15.2 / 13.6 / 12.0 / 11.2 / 9.6 / 8.0 / 6.4 / 4.8 / 3.2 / 1.6 / 0`。

**怎么验证的**：`testdata/rating_vectors.json` 取自水鱼公开的 `GET /api/maimaidxprober/player/test_data`（官方提供的测试数据，无需鉴权、不含任何凭证）。里面每条成绩都带服务端算好的 `ra`，以及服务端给出的 `rating` 总和。单测直接断言：

- 31 条 `ra` 向量逐条复现（覆盖 13 个系数档位）
- 用本实现的 `ra` 重算 b50，`rating` 与服务器值**完全相等**（示例数据为 15327）

唯一不满足公式的是**宴谱**（服务端把它们的 `ra` 置 0），本项目在上游就把宴谱排除，行为一致。

---

## 故障排查

排查顺序：先 `probe`，再 `verify --log-level debug`。日志里能看到每次机台调用的 api 名、hash、字节数（**不含**二维码与 token 全文）。

| 现象 | 判定 | 怎么办 |
| --- | --- | --- |
| `class=version_stale` | **配置的协议版本已不被接受** | 按提示换 `--version`，或用 `--auto-version`。换网络没用 |
| `class=blocked`（所有版本都 0 字节） | 三种成因：版本不被接受 / 触发封禁 / 出口 IP 被阻断 | 按顺序排除：换 `--version` → 等十几分钟（别再打）→ 换家宽 IP / 重启光猫 / 等 48–72 小时 |
| `UserLoginApi` 返回 **HTTP 500**（Tomcat 错误页） | 登录请求的字段形状与版本不符 | 检查 `internal/protocol/version.go` 里该版本的 `Login` 形状（字段名、时间单位、`isContinue`） |
| `class=params` | 收到的响应解不开 | `--version` 换一个版本 |
| `class=network` | TCP/TLS 建连失败 | 查 `--proxy`、防火墙、DNS 能否解析 `maimai-gm.wahlap.com:42081` |
| `class=business`，或 `returnCode=100` | 账号已在登录状态 | 会话残留。等约 15 分钟自动释放；**不要反复重试** |
| `returnCode=102` | 二维码过期 | 重新从公众号获取（有效期约 10 分钟） |
| `returnCode=110` | KeyChip 不匹配 | 机台参数问题，检查 chipID 与协议版本 |
| AimeDB `errorID=1` / `2` | 二维码过期或不存在 | 重新获取二维码 |
| AimeDB `errorID=50` | 签名错误 | 检查本机时区：时间戳按**本地时区**生成，时区不对签名就不对 |
| 「二维码格式非法」 | 输入不完整 | 确认 84 字符、`SGWCMAID` 开头、后 64 位是十六进制；图片链接也可以 |
| 水鱼 `导入token有误` | Import-Token 无效 | 去水鱼官网「编辑个人资料」重新生成 |
| 「登出未成功，账号会话可能在约 15 分钟内不可用」 | 登出请求失败 | 等会话超时；期间该账号登不上机台。检查网络稳定性 |
| 服务返回 401 | 服务令牌不对 | 确认客户端与服务端用的是同一个 `--token` |
| 服务返回 403 `write_disabled` | 写操作未开启 | 服务端加 `--allow-write`（想清楚再开） |
| 服务返回 429 | 触发限频 | 按 `Retry-After` 等待；限频保护的是出口 IP |
| 服务返回 503 `busy` | 并发占满 | 稍后重试；同账号的机台会话本就串行 |
| `verify` 条数为 0 | 账号没有游玩记录，或接口结构变了 | 换账号确认；若确信用过机台，检查是否机台号与手台号不同 |

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
                     server ─→ wsproto（零依赖 WebSocket 实现）
              rating  （叶子：ra 与 b50 计算，只依赖 model）
```

依赖是**单向**的，没有回环：`protocol` 不认识查分器，`sync` 不认识机台协议，`service` 是唯一同时依赖两边的编排层，`ops` 与 `server` 只认操作与错误码。

| 包 | 职责一句话 |
| --- | --- |
| `cmd/mai-arcade` | CLI 入口：解析参数、装配依赖、分发子命令 |
| `cmd/mai-arcade-server` | 服务入口：装配接入层、进程生命周期、优雅关闭 |
| `internal/service` | 编排「扫码 → 登录 → 取分 → 登出 → 同步」；版本探测、结果复用 |
| `internal/protocol` | 机台协议：二维码解析、AimeDB 换账号、标题服务器加解密与调用 |
| `internal/sync` | 查分器侧：`Syncer` 接口 + 注册表 + 水鱼实现（含合并） |
| `internal/chart` | `musicId ↔ 歌名/类型/定数/新旧曲` 索引，含本地缓存 |
| `internal/rating` | 单曲 ra 与 b50 计算 |
| `internal/transport` | HTTP 细节：超时、代理、响应上限、日志脱敏 |
| `internal/ops` | **与接入方式无关的操作层**：服务能做什么、写保护、参数校验 |
| `internal/server` | 接入层：HTTP 与 WebSocket 两个适配器，共用鉴权/限频/错误映射 |
| `internal/wsproto` | 零依赖的 WebSocket（RFC 6455）服务端：握手 + 帧编解码 |
| `internal/access` | 请求准入：Bearer 鉴权、令牌桶限频、体积与并发限额 |
| `internal/model` | 叶子包：成绩与枚举、错误分类（`Kind` 对应退出码）、错误码目录 |

### 为什么自己实现 WebSocket

Go 标准库没有 WebSocket，而本项目的核心决策之一是零第三方依赖（单静态二进制、可随时 scp 到别的网络环境、供应链干净）。服务端只需要握手与帧两件事，实现量可控，且能用 RFC 自带的官方样例断言；测试里另有一个**独立实现的客户端**与服务端互操作，而不是共用同一份可能出错的编解码。

### 接入方式与操作是分开的

`ops` 定义「能做什么」（`{id, op, args}` → 结果或错误码），`server` 只负责把消息搬进来、把结果搬出去。因此：

- 新增一个操作：只改 `ops`，HTTP 与 WebSocket **同时**获得该能力
- 新增一种接入方式：只加一个 `internal/server/<name>.go`，操作与安全策略一行都不用动
- 鉴权、限频、限额、错误码映射只有一份实现

---

## 常见任务索引（我要做 X → 改哪个文件）

| 我要做 | 改哪里 |
| --- | --- |
| **加一个协议版本**（游戏更新后） | `internal/protocol/version.go` 的 `Versions` 表加一项；若自动探测也要认它，同步更新 `internal/service/version.go` 的 `versionProbeOrder` 与 `protocol.DefaultVersion`；再往 `internal/protocol/crypto_test.go` 补 hash 对照值 |
| **确认当前该用哪个版本** | `make integration` 跑 `TestIntegrationDefaultProtocolVersionIsAccepted`，它逐个版本实测并报告 |
| **登录被服务端拒绝（HTTP 500）** | 对照 `internal/protocol/version.go` 的 `Version.Login`（字段名 / 时间单位 / `isContinue` / 登出 `type`）与社区参考实现的登录体逐项核对；改完补 `internal/protocol/title_test.go` 的按版本字段断言 |
| **加一个查分器** | 新增 `internal/sync/<名字>.go`，实现 `Name()` 与 `Upload()`，在 `init()` 里 `Register("名字", func(d Deps) Syncer { ... })` 一行。**既有文件零改动**——`internal/sync/fakesyncer_test.go` 是这条约定的活凭据 |
| **加一个机台 API** | `internal/protocol/api.go` 加 API 名常量与请求/响应结构体（字段名照抄官方，**含 `acsessCode` 这类官方拼写错误**）；在 `title.go` 加薄方法，内部复用 `call()`；补一条表驱动测试 |
| **加一个操作**（服务与 CLI 都能用） | 在 `internal/ops/operations.go` 写一个 `Run` 函数，在 `init()` 里 `Register(Operation{...})` 一行；需要写权限就设 `Mutating: true` |
| **加一个 CLI 子命令** | 新增 `cmd/mai-arcade/<名字>.go`，写 `run<Name>(ctx, *environment, args) error`；在 `main.go` 的 `commands` map 加一行 |
| **加一种接入方式**（如 gRPC） | 新增 `internal/server/<名字>.go`，把请求解成 `ops.Dispatch(ctx, deps, op, args)`；鉴权用 `access.Authenticator`，限频用 `access.RateLimiter` |
| **加一个错误码** | 在 `internal/model/codes.go` 加常量并列入 `DeclaredCodes()`，在拥有该错误的包里加哨兵并 `model.RegisterCode(...)`，最后在 `internal/server/server.go` 的 `codeStatus` 补状态映射（测试会强制你补） |
| **换协议版本默认值** | `internal/protocol/version.go` 的 `DefaultVersion` |
| **调超时 / 会话时长 / 限频** | CLI：`--timeout` / `--session-timeout`；服务：`--op-timeout` / `--rate-burst` / `--rate-interval`（默认值在 `cmd/mai-arcade-server/config.go`） |
| **改同步合并策略** | `internal/sync/merge.go` 的 `merge()`，配套改 `internal/sync/merge_test.go` |
| **改 rating 系数表** | `internal/rating/b50.go` 的 `coefficientTable`，跑 `internal/rating` 的真值测试确认 |
| **改错误码 → HTTP 状态映射** | `internal/server/server.go` 的 `codeStatus` |

---

## 安全须知

- **二维码（SGWCMAID）是账号唯一凭证**（1.53+）。有效期内持有者即可登录该账号。本项目做到了：不落盘、不写日志、不回显全文——`String()` 与 JSON 序列化都只输出「前缀 + 长度」，因此即使误塞进日志或结构化输出也不会泄露。
- **token 同理**（AimeDB token、水鱼 Import-Token、服务令牌）。日志里只出现前 4 位，或只说明来源。
- 二维码**只经 stdin 传入**（CLI），或放在 POST 请求体里（服务）——都不放 query，避免进访问日志。
- **结果复用缓存以二维码的哈希为键**，不保留原文：原文是凭证，不该在内存里多留一份。
- 服务默认**只读**且只监听回环地址；`--allow-write` 会改动真实查分器数据，开启前请想清楚。
- 日志走 stderr，结构化结果走 stdout，两者不混。
- **不要把二维码、token、真实成绩贴进 issue、聊天群或日志附件。**
- 本项目只操作用户自己扫码授权的账号。

---

## 开发

```bash
make test           # 全部单测，不访问网络
make test-race      # 带竞态检测（并发代码必过）
make test-isolated  # 在无网络的命名空间里跑单测，验证「单测不访问真实接口」
make lint           # gofmt 检查 + go vet（含 integration 标签下的文件）
make build          # 构建 bin/mai-arcade 与 bin/mai-arcade-server
make release        # 交叉编译 linux/amd64 与 linux/arm64
make integration    # 需要真实网络的集成测试；需要凭证的用例会自动跳过
```

测试规模：约 7.2k 行实现 + 7.9k 行测试，265 个测试函数 / 565 个用例，**零第三方依赖**（`go list -m all` 只有本模块一行）。

`make test-isolated` 不是摆设：它靠 `unshare -rn` 切断网络后重跑全部单测（`-count=1` 跳过缓存）。写这套测试时正是它抓出了真实缺陷——`sync` 原本在校验 `--site` 之前就去拉曲目索引，于是拼错的站点名会被报成网络故障而不是参数错误。

**目录约定**：`internal/` 保证外部无法 import；文件超过约 300 行或出现第二个职责就拆。

**代码风格**（详见原规范 §6）：代码表达意图，注释只讲「为什么」。

- 用类型与构造函数表达约束（`type SGID string` + `NewSGID()`，非法值进不了系统内部）
- 表驱动代替 `if/switch`（版本参数、枚举映射、错误码、HTTP 状态全是表）
- 小函数代替长注释（`loadRemote()` / `merge()` / `upload()`，函数名即文档）
- 依赖显式传入，不在函数体内读环境变量（服务令牌由装配层读取后传入）
- 只在「为什么」处写注释，例如：`// fc 传空会清空服务器标记，必须合并`
- 每个包有一句 `// Package xxx` 说明职责；每个导出符号有一句说明

### 集成测试与手动验证

需要真实凭证的用例受 `//go:build integration` 隔离，默认不跑：

```bash
# 只读验证：连通性、当前可用协议版本、曲目库结构（不写入任何数据）
make integration

# 需要二维码的完整链路验证（二维码约 10 分钟有效）
MAI_SGID='SGWCMAID...' make integration

# 需要水鱼凭证的合并验证（只读，不上传）
MAI_FISH_TOKEN='...' make integration
```

**同步验证（会写入真实账号，必须由人明确发起）**：建议先在一个测试账号上确认「`fc` 传空是否会被清空」这一前提是否仍然成立：

1. 在测试账号上保留至少一条带 FC 标记的成绩
2. 跑 `sync`，然后回查分器网页确认那条 FC 还在
3. 若被清空，说明合并策略失效，请检查 `internal/sync/merge.go`

这最后一步刻意**没有**自动化——它会改变真实账号的数据。

---

## 与原规范的差异

实现过程中发现文档有几处与实际不符，记录在此以免后来者被误导：

| 项 | 原规范 | 实测 / 实现 |
| --- | --- | --- |
| 默认协议版本 | 1.53（文档围绕 1.53 撰写） | **1.55**。1.53 已不被服务端接受，而失效症状是「HTTP 200 + 0 字节」，与 IP 阻断同形，会让所有人看到一个假的「IP 被阻断」 |
| 出口 IP 被阻断 | 列为第一约束，判定为阻塞项 | 实测该 IP **未**被阻断；原判定把「版本不被接受」误读成了 IP 问题。已改为逐个版本探活来区分两者，并新增 `version_stale` 一类 |
| b50 算法 | 标为「待验证」，只给了 ra 求和结论 | **已验证**：从官方测试数据反推出系数表并逐条复现，`rating` 与服务端完全相等 |
| §5.2「`sync` 不得 import `protocol`」与 §5.3 的 `Syncer` 签名用 `protocol.Score` | 两处自相矛盾 | 把共享数据模型与错误分类下沉到叶子包 `internal/model`，依赖方向仍是单向的 |
| `Syncer` 注册 | `init()` 里 `Register(&NewSite{})` | 注册**工厂函数**：水鱼实现需要注入曲目索引，零值实例无法满足。新增站点仍是「1 个文件 + 1 行注册」 |
| §2.5 登录/登出字段 | `acsessCode`（称官方源码拼错）、`dateTime` 毫秒、`isContinue:true`、登出只有 userId+loginDateTime | 1.55 实测该形状被服务端以 **HTTP 500** 拒绝。对照社区参考实现（`MAI_ENCODING=1.55`）改为：`accessCode`、**秒**级时间戳、额外带 `loginDateTime`、`isContinue:false`、`regionId:8`、登出带 `type`。这些差异按 §5.3 的要求做成了**版本参数表里的数据**（`Version.Login`），不写版本号分支 |
| §2.3 AimeDB 响应字段 | 只有 `errorID` / `userID` / `token` | 实测响应还带 **`key` 与 `timestamp`**（服务端签名材料）。已解析并保留在 `Credential` 中，不再静默丢弃 |
| §2.4 请求头 | 未提及 `number` 头 | 参考实现会带 `number: 0`；已补上 |
| §3.1 出口 IP 被阻断 | 列为第一约束 | 实测未阻断，原判定是把版本问题误读成 IP 问题；且 0 字节还有第三种成因——**请求过频触发的封禁**。已在 `probe` 提示与故障表里区分三者 |
| 目录结构 | 未包含 `model` / `chart` / `rating` / `ops` / `server` / `access` / `wsproto` | `chart` 是被协议现实逼出来的（水鱼按**歌名**匹配曲目，机台只给 `musicId`，同步必须做这层翻译）；`ops` / `server` / `access` / `wsproto` 服务于 P2 的服务化与「接入方式可插拔」；`pkg/` 未创建，因为本项目以二进制对外提供能力，无需暴露 Go API |

---

## 许可与声明

**风险声明**：本项目使用社区逆向得到的协议，**无官方授权渠道**。使用可能导致**账号被限制、出口 IP 被封禁（最长 48–72 小时）**。请自行评估风险。

**使用边界**：只操作用户自己扫码授权的账号。禁止批量扫号、禁止成绩伪造、禁止写入他人数据、禁止用于商业爬取。

**不做什么**：不做机台出码（生成二维码需要注入微信客户端，属于另一个项目）；不实现高风险写接口（改门、发票券、写收藏品）。
