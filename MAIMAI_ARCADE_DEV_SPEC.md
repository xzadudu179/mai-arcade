# 舞萌 DX 机台协议库（Go）—— 开发需求与规范

> 本文档用于**开新工作区**实现一个独立的机台协议库：通过玩家二维码（SGWCMAID）拿到机台账号与**完整成绩（含 FC/FS）**，并同步到查分器。
> 本文档自包含：协议参数、算法、字段、约束、接口设计、开发规范、扩展指南、验收标准均在文内，不依赖任何既有仓库。
>
> 状态图例：**[已确证]** 有官方/多份开源实现交叉验证 · **[已验证]** 本项目实测通过 · **[待验证]** 尚未实测 · **[推测]** 由间接证据推断

---

## 1. 背景、目标与选型

### 1.1 背景

水鱼（Diving-Fish）与落雪（LXNS）查分器的公开 API **都没有二维码入口**，只能读库里已有成绩。而现有同步渠道存在三个硬伤：

- 通过查分器 API 同步的成绩**拿不到竞速标识**（FC / FC+ / AP / AP+ / FS / FS+ / FSD / FSD+ / SYNC）
- 1.53 更新后机台登录改为**二维码唯一凭证**，旧的「仅凭 userId 拉成绩」方式失效
- 现成封装库（如 `maimai-py`）走的是**对手成绩接口**（`GetUserRivalMusicApi`），源码里直接写死 `fc=None, fs=None`

因此需要**自行实现机台协议**，走官方完整成绩接口（`GetUserMusicApi`）拿全字段。

### 1.2 目标

| 优先级 | 目标 |
| --- | --- |
| P0 | SGWCMAID 二维码换机台账号（userId + token） |
| P0 | 登录会话、拉取**全量成绩**（含 FC/FS），支持分页 |
| P0 | 正确登出（否则进 15 分钟「小黑屋」） |
| P0 | 成绩转水鱼格式并推送，**保护原有 FC/FS 不被清空** |
| P0 | 连通性自检：能区分「网络不通 / IP 被阻断 / 参数错 / 业务错」 |
| P1 | 落雪同步；CLI 子命令；账号资料拉取；b50 计算 |
| P2 | 本地 HTTP 服务模式；多版本参数自动探测；结果缓存 |

### 1.3 非目标

- 不做**机台出码**（生成二维码需注入微信客户端，如 `maiserver` 项目）。本项目只**消费**用户从公众号拿到的二维码。
- 不做批量扫号、全服数据库、成绩伪造、未授权写入。**只操作用户自己扫码授权的账号**。
- 不实现高风险写接口（改门、发票券、写收藏品）。

### 1.4 语言选型：Go

**[已验证]** Go 侧 `apiHash` 实现已用 1.50 官方已知向量断言通过，且与 Python 实现逐条一致（见 §2.4）。

选 Go 的决定性理由是**部署形态**，而非性能：

| 维度 | Go | Python |
| --- | --- | --- |
| 依赖 | **零第三方**（`crypto/aes`、`crypto/md5`、`crypto/sha256`、`compress/zlib`、`net/http` 全在标准库） | 需 `pycryptodome` + HTTP 库 |
| 部署 | **单静态二进制**，`scp` 过去即可运行，可交叉编译到 arm64 等 | 需 Python 3.10+、venv、依赖安装 |
| 换网络环境 | 随时可搬到另一条家宽 / 树莓派 / 路由器旁设备 | 每台机器都要重装环境 |
| 供应链 | 无第三方依赖，处理凭证的项目更干净 | 第三方依赖需跟进安全更新 |

**为什么「能换环境」是本项目的关键**：出口 IP 可能被华立阻断（§3.1），届时必须把服务搬到别处。零依赖单二进制让这件事变成一条 `scp`。

已知代价（可接受）：
- **没有任何 Go 现成实现可抄**（社区只有 Python / Rust / TS 版本）→ 但协议参数已在 §2 逐项确认并核验，不依赖抄代码
- 本仓库的 bot 是 Python，调用方式改为**子进程**（§5.5），二维码经 **stdin** 传入（不放 argv）

---

## 2. 协议事实（已确证）

### 2.1 整体链路

```
用户从公众号获取 SGWCMAID（84 字符，约 10 分钟有效）
        │
        ▼
[1] AimeDB：POST http://ai.sys-allnet.cn/wc_aime/api/get_data
        │   → userId + token（+ Set-Cookie）
        ▼
[2] 标题服务器：POST https://maimai-gm.wahlap.com:42081/Maimai2Servlet/{api_hash}
        │   UserLoginApi   （用 token 登录）
        │   GetUserMusicApi（分页拉全量成绩，含 comboStatus/syncStatus）
        │   UserLogoutApi  （必须登出，且用与登录相同的 dateTime）
        ▼
[3] 转换为查分器格式 → 推送水鱼 / 落雪
```

### 2.2 SGWCMAID 格式 **[已确证]**

```
SGWCMAID + 12 位时间戳(YYMMDDHHMMSS) + 64 位大写十六进制签名 = 84 字符
```

- 校验：长度 84 && 前缀 `SGWCMAID` && `[20:]` 全为 `[0-9A-F]`
- 公众号二维码图片是个链接，从中提取需**补回 `SGWC` 前缀**：
  - `https://wq.wahlap.net/qrcode/req/MAID...`（或 `/qrcode/img/`）
  - 正则：`wq\.wahlap\.net/qrcode/(?:req|img)/(MAID[^?.\s]+)` → `"SGWC" + match`
- **有效期约 10 分钟**；登录会话约 30 分钟；服务器会话硬超时 15 分钟（超时未登出即进「小黑屋」）

### 2.3 AimeDB **[已确证]**（连通性 **[已验证]**）

| 项 | 值 |
| --- | --- |
| URL | `http://ai.sys-allnet.cn/wc_aime/api/get_data` |
| 方法 | POST，`Content-Type: application/json` |
| User-Agent | `WC_AIME_LIB` |
| chipID | `A63E-01C28055905`（1.53 起；旧版 `A63E-01E68606624`） |
| openGameID | `MAID` |
| qrCode | **只传二维码后 64 位**（`sgid[20:]`） |
| timestamp | `YYMMDDHHMMSS`，本地时区（Rust 版显式 UTC+8） |
| key | `sha256(chipID + timestamp + commonKey)` 的 hex（**大写**） |
| commonKey | `XcW5FW4cPArBXEk4vzKz3CIrMuA5EVVW`（32 字符，逐字符核对，中间有 `A5`） |

请求体（紧凑 JSON）：

```json
{"chipID":"A63E-01C28055905","openGameID":"MAID","key":"<SHA256大写>","qrCode":"<64位>","timestamp":"<YYMMDDHHMMSS>"}
```

响应：`{"errorID": 0, "userID": 10807675, "token": "..."}`

`errorID` 语义：

| errorID | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 二维码过期（30 分钟档） |
| 2 | 二维码过期（10 分钟档） |
| 50 | 签名错误 |

> 60001 / 60002 是社区库自定义的本地错误码（格式不合法 / 响应无法解析），非服务器返回。

**实测**：本机连通成功（HTTP 200 + 业务响应，用过期二维码得 `errorID=1`），说明请求格式与签名算法被服务器接受。

### 2.4 标题服务器 **[已确证]**（连通性 **[待验证]**）

| 项 | 值 |
| --- | --- |
| Base URL | `https://maimai-gm.wahlap.com:42081/Maimai2Servlet/` |
| 完整 URL | `{Base}{api_hash}`（**无额外路径段**） |
| api_hash | `hex(md5(api_name + "MaimaiChn" + ObfuscateParam))`（小写） |
| api_name | **API 变体名原样**：`Ping` / `UserLoginApi` / `GetUserMusicApi` / `UserLogoutApi` … |
| 当前可用版本 | **1.55**（实测 Ping 通过、响应可解密；1.53 与 1.56 均被服务端静默丢弃） |
| 请求体 | `AES-256-CBC(PKCS7, zlib.compress(json))`（1.53+ 顺序：先压缩后加密） |
| 响应体 | AES-CBC 解密 → 若明文以 `\x78\x9c` 开头则 `zlib.decompress` |

请求头：

```
User-Agent:        {api_hash}#{agent_id}      # agent_id = userId
Content-Type:      application/json
Mai-Encoding:      {版本号，如 1.53 / 1.55}
Charset:           UTF-8
Content-Encoding:  deflate
Accept-Encoding:   （空字符串，显式禁用）
Expect:            100-continue
```

**各版本参数表**：

| 版本 | ObfuscateParam | AES-256 Key（32 字节） | AES IV（16 字节） |
| --- | --- | --- | --- |
| 1.40 | `BEs2D5vW` | `n7bx6:@Fg_:2;5E89Phy7AyIcpxEQ:R@` | `;;KjR1C3hgB1ovXa` |
| 1.50–1.52 | `B44df8yT` | `a>32bVP7v<63BVLkY[xM>daZ1s9MBP<R` | `d6xHIKq]1J]Dt^ue` |
| 1.53 | `LatuAa81` | `o2U8F6<adcYl25f_qwx_n]5_qxRcbLN>` | `AL<G:k:X6Vu7@_U]` |
| 1.55 | `8bF76dE9` | `FKM2JX:VjZNK6hc:A0<JU:i5oR7LA]9W` | `F>;24DjU9W6ZsRH[` |

> 后缀统一为 `"MaimaiChn" + ObfuscateParam`，如 1.53 是 `MaimaiChnLatuAa81`。
> **1.40–1.52 的加密顺序相反**（先 AES 后 zlib）；本项目只需支持 1.53+。

**api_hash 自测向量（1.50 参数）** —— Go 与 Python 两种实现均已断言通过：

```
md5("Ping"              + "MaimaiChnB44df8yT") == 250b3482854e7697de7d8eb6ea1fabb1
md5("GetUserPreviewApi" + "MaimaiChnB44df8yT") == 004cf848f96d393a5f2720101e30b93d
md5("GetUserDataApi"    + "MaimaiChnB44df8yT") == 3af1e5b298bb5b7379c94934b2e038c5
```

1.53 / 1.55 下的常用 hash（两种实现结果一致，供调试对照）：

| api_name | 1.53 (`LatuAa81`) | 1.55 (`8bF76dE9`) |
| --- | --- | --- |
| `Ping` | `5c39ae037195be3f0f9a8e8f6f8ec849` | `3bae8c1162b3148abcfc38a89c03e3e7` |
| `UserLoginApi` | `f9d6e44dd1a72ff9ff56efbc2f77fe8b` | `04dd218d2070685e728719b974f9b8c0` |
| `GetUserMusicApi` | `9497365ea1a6caf7789cee131dfd3017` | `0b31f4a74d0b4eb07ef53e036625ed89` |
| `UserLogoutApi` | `4329b8dc275ea277243c26185bee35c7` | `aaa0817a628ae9cc3df35b120ecb33a4` |

### 2.5 核心 API **[已确证]**

#### UserLoginApi

**字段形状随版本变化，必须按版本参数化（不可硬编码一套）**：

| 字段 | 1.53 及更早 | 1.55 |
| --- | --- | --- |
| 访问码键名 | `acsessCode`（官方源码拼错的那份） | `accessCode`（拼写正确） |
| `dateTime` 单位 | 毫秒 | **秒** |
| 是否带 `loginDateTime` | 否 | 是（与 `dateTime` 同值） |
| `isContinue` | `true` | `false` |
| `regionId` | 0 | 8 |
| 登出请求 | 只有 `userId` + `loginDateTime` | 额外带 `type: 5` |

1.55 的请求体示例：

```json
{"userId": 10807675, "regionId": 8, "dateTime": 1758364800,
 "accessCode": "", "placeId": "", "clientId": "",
 "token": "<AimeDB 返回的 token>", "isContinue": false,
 "loginDateTime": 1758364800, "genericFlag": 0}
```

- **用 1.53 的字段形状打 1.55 服务端会得到 `HTTP 500`（Tomcat 错误页，非业务返回码）**——实测观测
- 字段名必须逐字符一致：把 `acsessCode` 当成拼写错误去「修正」会导致缺字段被拒
- `dateTime` 登出时必须传相同值
- `returnCode`：`1` 成功 / `100` 已登录 / `102` 二维码过期 / `110` KeyChip 不匹配

> 实现要求：这些差异**只能以数据形式存在**（版本参数表里的 `LoginShape`），业务代码里禁止写 `if version == "1.55"`。详见 §5.3。

#### GetUserMusicApi（分页）

请求 `{"userId": 10807675, "nextIndex": 0, "maxCount": 50}`（Rust 版用 2000，建议取大以减少轮次；上限 **[待验证]**）

响应 `{"nextIndex": <int>, "userMusicList": [{"userMusicDetailList": [ ... ]}]}`

- 终止：`nextIndex == 0` 或 `userMusicList` 为空
- 过滤：`playCount <= 0` 丢弃

成绩明细字段：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `musicId` | int | `<10000` SD 谱；`>=10000` DX 谱；`>=100000` 宴谱（同步跳过） |
| `level` | int | 0=Basic, 1=Advanced, 2=Expert, 3=Master, 4=Re:Master, 5=宴 |
| `playCount` | int | 游玩次数 |
| `achievement` | int | **达成率 ×10000 的整数**（`1010000` = 101.0000%） |
| `comboStatus` | int | 0=无, 1=FC, 2=FC+, 3=AP, 4=AP+ |
| `syncStatus` | int | 0=无, 1=FS, 2=FS+, 3=FSD, 4=FSD+, 5=SYNC |
| `deluxscoreMax` | int | **DX 分数**（拼写是小写 `s`） |
| `scoreRank` | int | 0=D … 13=SSS+ |
| `extNum1`/`extNum2` | int | 扩展值 |

#### UserLogoutApi（**必须调用**）

```json
{"userId": 10807675, "loginDateTime": 1758364800000}
```

`loginDateTime` 必须等于登录 `dateTime`，否则登出失败 → 会话残留 15 分钟 → 期间无法再登录。

### 2.6 查分器同步 **[已确证]**

#### 水鱼

| 项 | 值 |
| --- | --- |
| 校验 | `GET /api/maimaidxprober/player/records`，头 `Import-Token: <token>` |
| 上传 | `POST /api/maimaidxprober/player/update_records`，头同上 |
| 请求体 | **裸 JSON 数组**（不是 `{"records": [...]}`） |

```json
[{"achievements": 101.0000, "dxScore": 2711, "fc": "fc", "fs": "",
  "level_index": 3, "title": "Alea jacta est!", "type": "SD"}]
```

服务端校验规则（**决定合并策略**）：

| 字段 | 规则 |
| --- | --- |
| `achievements` | 超 101.0 截断；非数字则整个请求失败 |
| `dxScore` | 无校验；非数字置 0 |
| `fc` | **值不在 `fc/fcp/ap/app` 内会被设置为空** |
| `fs` | **值不在 `sync/fs/fsp/fsd/fsdp` 内会被设置为空** |
| `level_index` | 必须是该曲真实存在的难度，否则**该条被跳过** |
| `title` | **以歌名精确匹配曲目**（不是 song_id），否则跳过 |
| `type` | 必须是该曲真实存在的 `SD`/`DX`，否则跳过 |

> ⚠️ **必须做合并上传**：`fc`/`fs` 传空会**清空**服务器已有标记，而机台数据在部分情况下拿不到 FC/FS。流程必须是「**先读现状 → 按 (title, type, level_index) 合并（机台缺的字段保留原值）→ 再上传**」。
> 同名曲：`Link`(131) 与 `Link(CoF)`(383) 的 `title` 必须写 `Link(CoF)` 才能匹配后者。

#### 落雪 **[待验证]**

`POST /api/v0/user/maimai/player/scores`，头 `X-User-Token: <个人 token>`。

---

## 3. 硬性约束与风险

### 3.1 版本必须精确匹配（**第一排查项**）

`Mai-Encoding`、`api_hash`、AES 参数**三者必须来自同一版本**，且该版本必须是服务端当前接受的版本。

**[已验证] 实测对照实验**（同一出口 IP、同一路径，只改 `Mai-Encoding`）：

| 请求 | 响应 | 说明 |
| --- | --- | --- |
| 1.53 hash + `Mai-Encoding: 1.53` | `200` + **0 字节** | 版本不被接受 → 静默丢弃 |
| 1.53 hash + `Mai-Encoding: 1.55` | `404` + HTML | hash 与版本不配套 |
| **1.55 参数 + `Mai-Encoding: 1.55`** | `200` + 32 字节 → `{"result":"Pong"}` | ✅ **正常** |
| 1.55 参数 + `Mai-Encoding: 1.56` | `200` + **0 字节** | 版本号必须精确匹配 |

**四条结论**：

1. **当前可用版本 = 1.55**（与查分器返回的 `lastDataVersion: 1.55.03` 一致；游戏 ROM 显示 1.56，但协议版本号是 1.55）
2. **版本不被接受的表现是 `HTTP 200 + 0 字节`，与「IP 被阻断」在传输层完全同形** → 排查顺序必须是「**先换版本号，再怀疑 IP**」
3. 0 字节还有**第三种成因**：**请求过频触发的临时封禁**（判定方式：等十几分钟不再请求，若恢复即为过频）
4. 版本不配套还有 `404` 形态：hash 属于 A 版本而 `Mai-Encoding` 声明 B 版本

> ⚠️ **本项目踩过的坑（务必避免重复）**：最初用 1.53 参数测试，四种请求（含乱写 hash、根路径、明文体）全部 `200 + 0 字节`，据此**误判为「IP 被华立阻断」并写进文档第一约束**。真相是版本不被接受——换成 1.55 后同一 IP 立即返回 `Pong`。社区确实有 IP 被封的记录，但那不是本例的成因。

### 3.2 IP 阻断与过频封禁（仅在换遍版本仍空响应时考虑）

若**所有**已知版本都返回 `200 + 0 字节`，再考虑下面两种：

| 成因 | 判定 | 处置 |
| --- | --- | --- |
| 出口 IP 被华立阻断 | 换遍版本仍全空；社区记录见 maimai.py issue #21 | 换家宽 IP / 重启光猫 / 等 48–72 小时 |
| 请求过频触发临时封禁 | 停止请求十几分钟后恢复 | 降频、避免无脑重试 |

社区对 IP 阻断的描述（maimai.py issue #21）：

> 被阻断的标志为，状态码返回 200，但 Response Body 始终为空……主流云服务器提供商大部分 IP 默认就在黑名单中，请使用家宽 IP 或者代理池。

### 3.2 频率与会话约束

| 约束 | 数值 | 后果 |
| --- | --- | --- |
| 二维码有效期 | ~10 分钟 | 过期需重新获取 |
| 登录会话有效期 | ~30 分钟 | — |
| 服务器会话硬超时 | 15 分钟 | 未正常登出即进「小黑屋」 |
| 同账号并发登录 | 不允许 | 互相挤掉 / 报「用户正在登录中」 |
| IP 请求频率 | 阈值未知 | 触发激进封禁（整条线路受影响，最长 48–72 小时） |

**因此**：同账号操作必须**串行化**（Go 用 `sync.Mutex` 或单 goroutine 队列）；每次用完必须登出；**禁止在循环里无脑重试**（会加重封禁）。

### 3.3 凭证安全

- **SGWCMAID 是账号唯一凭证**（1.53+），有效期内持有者可登录该账号 → **绝不落盘、绝不写日志、绝不回显全文**
- AimeDB 的 `token`、水鱼 `Import-Token`、落雪 token 同理
- 日志只允许出现：前缀 + 长度、成绩条数等统计值
- **CLI 的二维码经 stdin 传入，不走命令行参数**（`ps` 会暴露 argv）

### 3.4 合规

- 协议来自社区公开实现，**无官方授权渠道 [已确证：未找到]**
- 只操作用户**自己扫码授权**的账号；禁止批量扫号、禁止写入他人数据
- 项目须保留风险声明（账号可能被限制、IP 可能被封）

---

## 4. 功能需求

### 4.1 P0

| 编号 | 需求 | 要点 |
| --- | --- | --- |
| F1 | 二维码解析 | 接受纯文本或公众号链接；补 `SGWC` 前缀；校验格式与时间戳新鲜度（>10 分钟提示重新获取） |
| F2 | 扫码换账号 | AimeDB；错误码映射为可读提示 |
| F3 | 登录会话 | `UserLoginApi`；失败给出原因（已登录 / 过期 / keychip 不匹配） |
| F4 | 全量拉成绩 | `GetUserMusicApi` 分页；过滤 `playCount<=0` |
| F5 | 正常登出 | 相同 `dateTime`；**异常路径也要登出**（`defer`） |
| F6 | 转水鱼格式 | `achievement/10000`、`level_index`、`fc`/`fs` 字符串、`type`、`title` |
| F7 | 合并上传 | 先读现状 → 合并（机台缺的 FC/FS 保留原值）→ 上传；宴谱跳过 |
| F8 | 连通性自检 | 区分四类故障（§5.6） |

### 4.2 P1

| 编号 | 需求 |
| --- | --- |
| F9 | 落雪同步 |
| F10 | CLI 子命令（`probe` / `verify` / `sync`） |
| F11 | 账号资料拉取（头像 / 姓名框 / 牌子 / 称号 / 评级） |
| F12 | 成绩转 b50（新曲 15 + 旧曲 35 按 ra 降序；`rating` = 50 条 ra 之和）※ 算法已在水鱼侧验证：官方 B15 全为新曲、B35 全为旧曲、ra 之和 = rating |

### 4.3 P2

| 编号 | 需求 |
| --- | --- |
| F13 | 本地 HTTP 服务模式（带鉴权与限频） |
| F14 | 多版本参数自动探测（依次尝试 1.55 / 1.53，记录可用版本） |
| F15 | 同一二维码在有效期内复用会话（避免重复登录） |

---

## 5. 架构设计

### 5.1 设计目标：好加功能、好被接管

三条硬要求，贯穿所有设计决策：

1. **加新功能不改老代码**：新增查分器 / 新增 API / 新增 CLI 命令，都只**新增文件 + 注册一行**，不动既有逻辑（§5.3 扩展点）
2. **接管者 30 分钟内能上手**：目录结构自解释、命名说明意图、README 有「我要做 X 该改哪」的索引（§11）
3. **边界一眼可见**：`internal/` 分层、接口定义在调用方、依赖方向单向

### 5.2 目录结构

```
maimai-arcade/
├── cmd/
│   └── mai-arcade/
│       └── main.go              # 唯一入口：解析参数、装配依赖、分发子命令；不含业务
├── internal/                    # internal 保证外部无法 import，边界清晰
│   ├── protocol/                # 协议层：只懂机台协议，不懂查分器
│   │   ├── version.go           # 版本参数表（4 套 AES/混淆参数）+ 版本选择
│   │   ├── crypto.go            # apiHash / AES-CBC / zlib 打包解包（纯函数）
│   │   ├── sgid.go              # 二维码解析、格式校验、新鲜度判断（纯函数）
│   │   ├── aime.go              # AimeDB 客户端（扫码换账号）
│   │   ├── title.go             # 标题服务器客户端（call / login / logout / 拉成绩）
│   │   ├── api.go               # API 名常量 + 各 API 的请求/响应结构体
│   │   ├── score.go             # 成绩模型 + 枚举（Combo/Sync/Rank）映射
│   │   └── errors.go            # 错误类型 + 错误码到原因的映射
│   ├── sync/                    # 同步层：只懂查分器，不懂机台协议
│   │   ├── syncer.go            # Syncer 接口 + 注册表（扩展点）
│   │   ├── divingfish.go        # 水鱼实现（含合并逻辑）
│   │   └── lxns.go              # 落雪实现
│   ├── transport/               # 传输层：HTTP 细节集中于此
│   │   └── http.go              # 超时、代理、请求日志脱敏
│   └── service/                 # 编排层：串起「扫码→登录→取分→登出→同步」
│       ├── service.go           # 对外门面，调用方只用这一层
│       └── probe.go             # 连通性自检（四类故障判定）
├── pkg/                         # 需给外部 Go 程序 import 时才放这里（当前可空）
├── testdata/                    # 测试数据（假二维码、样例响应；禁止真实凭证）
├── README.md                    # 面向人类：快速开始、架构、常见任务索引（§11）
├── go.mod
└── Makefile                     # build / test / lint / cross-compile
```

**依赖方向（单向，禁止回环）**：

```
cmd  →  service  →  sync  ─┐
              │             ├→  transport
              └→  protocol ─┘
```

- `protocol` **不得** import `sync` / `service` / `cmd`
- `sync` **不得** import `protocol`
- `service` 是唯一同时依赖两边的层（编排职责）
- `transport` 谁都能用，但它不认识任何业务概念

### 5.3 扩展点（「加功能只新增文件」的实现方式）

#### ① 新增一个查分器

```go
// internal/sync/syncer.go —— 接口由调用方定义（不是实现方）
type Syncer interface {
    Name() string
    // Upload 只负责上传；合并策略由各实现自己决定（水鱼必须合并）
    Upload(ctx context.Context, scores []protocol.Score, credential string) error
}
```

新增 `internal/sync/newsite.go` 实现接口，并在 `init()` 注册：

```go
func init() { Register(&NewSite{}) }
```

`service` 通过 `sync.ByName("divingfish")` 取用，**不需要改任何既有文件**。

#### ② 新增一个机台 API

1. `internal/protocol/api.go` 加 API 名常量与请求/响应结构体
2. 需要的话在 `title.go` 加一个薄方法（内部走同一个 `call()`）
3. 表驱动测试加一行用例

#### ③ 新增一个 CLI 子命令

```go
// cmd/mai-arcade/main.go 只有注册与分发
var commands = map[string]func(context.Context, []string) error{
    "probe":  runProbe,
    "verify": runVerify,
    "sync":   runSync,
}
```

新增命令 = 新增 `cmd/mai-arcade/<name>.go` + 在 map 加一行。**主流程不动**。

#### ④ 新增一个协议版本（如 1.56）

只在 `internal/protocol/version.go` 的参数表加一项：

```go
var Versions = map[string]Version{
    "1.56": {Encoding: "1.56", ObfuscateParam: "xxxx", Key: []byte("..."), IV: []byte("...")},
}
```

> 设计约束：**版本差异只允许以数据形式存在**，禁止在业务代码里写 `if version == "1.53"`。

### 5.4 数据模型（关键结构）

```go
type SGID string         // 已校验的二维码内容；由构造函数保证合法性，零值不可用
type Credential struct { // AimeDB 返回，含敏感 token
    UserID int
    Token  string
}
type Score struct {      // 机台成绩（已归一化）
    MusicID     int
    Level       LevelIndex  // 0..4
    Achievement float64     // 已从 ×10000 转换
    DXScore     int
    Combo       ComboStatus // 0..4
    Sync        SyncStatus  // 0..5
    PlayCount   int
}
type Syncer interface{ ... }   // 见 §5.3
```

用**自定义类型 + 构造函数**表达约束（如 `NewSGID(raw string) (SGID, error)`），让非法值无法进入系统内部——比在注释里写「必须传合法二维码」可靠得多。

### 5.5 对外形态与 bot 集成

**CLI（主形态）**：

```bash
mai-arcade probe                                    # 连通性自检，无需凭证
mai-arcade verify                                   # 读 stdin 的二维码，只打印字段结构（脱敏）
echo "$SGID" | mai-arcade sync --fish-token xxx     # 二维码走 stdin，不出现在 ps
```

**Python bot 集成**（本仓库后续使用）：

```
subprocess.run(["mai-arcade", "sync", "--fish-token", token],
               input=sgid.encode(), capture_output=True, timeout=180)
```

约定：**退出码表达结果**（0 成功 / 1 业务失败 / 2 网络或阻断 / 3 参数错），**结构化结果走 stdout（JSON）**，日志走 stderr。调用方不用解析人类可读文本。

**P2 服务化**：加 `cmd/mai-arcade-server`，复用 `service` 层，提供 `POST /sync`（Bearer 鉴权 + 限频）。核心代码零改动。

### 5.6 自检命令的输出契约

`probe` 必须能区分四类故障（排查部署问题的唯一手段）：

| 现象 | 判定 | 退出码 |
| --- | --- | --- |
| 无法建立 TCP/TLS | 网络不通（检查代理/防火墙） | 2 |
| 响应 `200 + 0 字节` | **先逐个换版本号**；全版本都空才怀疑 IP 阻断或过频封禁 | 2 |
| 响应 `404` + HTML | hash 与 `Mai-Encoding` 版本不配套 | 3 |
| 收到响应但解密失败 | 协议参数版本不匹配（换 `--version`） | 3 |
| 收到业务错误码 | 业务问题（按错误码表提示） | 1 |

---

## 6. 开发规范

### 6.1 注释策略：**代码表达意图，注释只讲「为什么」**

本项目要求**少写注释**，把意图放进代码结构里：

| 做法 | 例子 |
| --- | --- |
| **类型表达约束** | `type SGID string` + `NewSGID()` 校验；`type ComboStatus uint8` 而非裸 `int` |
| **命名表达意图** | `apiHash(apiName, obfuscateParam)`；`mergePreservingMarks(local, remote)`；避免 `calc` / `data` / `tmp` |
| **表驱动代替分支** | 版本参数、枚举映射、错误码全部写成 `map` / `slice` 常量，而非散落的 `if/switch` |
| **小函数代替长注释** | 把「先读现状再合并」拆成 `loadRemote()` + `merge()` + `upload()`，函数名即文档 |
| **测试即行为文档** | 表驱动测试的 `name` 写清场景（`"机台无fc时应保留水鱼原值"`），比注释可靠 |
| **只在「为什么」处写注释** | 如 `// 1.53 起改为先压缩后加密，与 1.40 相反`、`// fc 传空会清空服务器标记，必须合并` |

**反例（禁止）**：

```go
// 计算 api hash                          ← 函数名已经说了
// apiName 是 API 名称，obfuscate 是参数    ← 参数名已经说了
func apiHash(apiName string, obfuscate string) string { ... }
```

**正例**：

```go
// 1.53 起改为先 zlib 压缩再 AES 加密，与 1.40 相反
func packBody(payload any, v Version) ([]byte, error) { ... }
```

**硬性要求**：
- 每个**包**必须有一句 `// Package xxx ...` 说明职责边界（唯一鼓励写详细的注释）
- 每个**导出符号**必须有一句说明行为与返回值
- 内部函数**默认不写注释**，除非有不显然的约束（协议坑、顺序要求、单位约定）

### 6.2 代码组织

- **单一职责**：一个函数做一件事；一个文件一个主题；文件超 ~300 行或出现第二个职责就拆
- **依赖显式化**：代理地址、超时、凭证一律从参数传入；不在函数体内读全局变量或环境变量
- **逻辑与副作用分离**：先纯计算（构造请求体、算出合并结果），再集中发 IO
- **收敛复用**：同一逻辑出现 ≥2 处抽公共函数；只出现 1 处不提炼
- **命名**：包名小写单词、导出标识符首字母大写、错误变量 `ErrXxx`；布尔判断用 `is`/`has` 前缀

### 6.3 错误处理

- 定义哨兵错误或自定义错误类型，**不要只返回字符串**
- 错误包装用 `fmt.Errorf("...: %w", err)` 保留链路
- **禁止无脑重试**：仅对明确可重试的情况（解密失败、超时）重试，且有次数上限与退避
- 用户可见的错误要说清**下一步怎么办**（如「IP 被阻断，请换网络」），而非只报 code

### 6.4 日志

- 用标准库 `log/slog` 输出结构化日志到 **stderr**
- **脱敏（硬性）**：SGID 只打前 8 位 + 长度；token 只打前 4 位；请求/响应体只打大小与字段名
- 级别约定：`Info` 关键步骤（登录成功、拉到 N 条）、`Debug` 协议细节（hash、字节数）、`Error` 需人工介入

### 6.5 变更流程

1. 动手前先读目标文件当前内容
2. 改函数签名时 grep 全部调用点一并更新
3. 改完必写/更新测试（正常 + 边界 + 异常输入）
4. 提交前 `make test`（含 `-race`）与 `gofmt` / `go vet`
5. 提交信息说明「做了什么」

---

## 7. 测试要求

### 7.1 单元测试（`go test`，不访问网络，必须覆盖）

| 对象 | 测试点 |
| --- | --- |
| `protocol.apiHash` | **用 §2.4 三条官方向量断言**（1.50）；1.53/1.55 对照值 |
| `protocol` 加解密 | 打包 → 解包往返一致；带/不带 zlib 头的响应都能解；错误 key 必须报错 |
| `protocol.NewSGID` | 纯文本 / req 链接 / img 链接 / 大小写 / 前后空格；**非法输入必须被拒绝**（长度≠84、前缀错、`[20:]` 含非十六进制） |
| `protocol` 新鲜度 | `YYMMDDHHMMSS` 解析；超 10 分钟判过期；未来时间容错 |
| `protocol.Versions` | 参数表完整性（每版本 4 字段齐全、Key 长度 32、IV 长度 16） |
| 成绩转换 | `achievement` ×10000 → 百分比；`comboStatus`/`syncStatus` → 字符串；宴谱过滤 |
| `sync` 合并 | **构造「远端有 fc / 机台无 fc」断言 fc 被保留**；不同曲不串行；`level_index` 越界条目跳过 |
| b50（若实现） | 新 15 + 旧 35；`rating` = ra 之和；不足时截断行为 |

要求：**表驱动**（`tests := []struct{ name string; ... }`），用例名写清场景；并发相关代码用 `-race` 跑。

### 7.2 集成测试（需网络与有效二维码，手动触发）

- `mai-arcade probe`：连通性（无需凭证）
- `mai-arcade verify`：完整链路，断言成绩条数 > 0 且含 `comboStatus`/`syncStatus`
- 同步验证：**先用测试账号确认 `fc` 空值是否会被清空**，据此确认合并策略必要性

### 7.3 测试纪律

- 单测**禁止访问真实接口**（用 `httptest.Server` 或注入的 stub transport）
- 二维码 / token **不得写入测试文件或 `testdata/`**，用格式合法的假数据
- 需要真实凭证的验证放 `//go:build integration` 标签下

---

## 8. 构建、部署与运维

| 项 | 要求 |
| --- | --- |
| Go 版本 | 1.22+（验证环境为 1.25.5） |
| 依赖 | **标准库**（`crypto/aes`、`cipher`、`md5`、`sha256`、`compress/zlib`、`net/http`、`log/slog`、`context`） |
| 构建 | `make build` → 单一二进制 `mai-arcade` |
| 交叉编译 | `make release` 产出 linux/amd64、linux/arm64（方便搬到树莓派等设备） |
| 网络 | 家宽 IP 优先；支持 `--proxy`；启动前跑 `probe` |
| 并发 | 同账号串行（互斥）；全局单实例 |
| 限频 | 调用方负责；建议同一用户 5 分钟 ≤3 次 |
| 监控 | 记录成功率；连续失败自动停止重试（避免加重封禁） |
| 退出码 | 0 成功 / 1 业务失败 / 2 网络或阻断 / 3 参数错 |

---

## 9. 验收标准

- [ ] `probe` 能区分四类故障（网络 / 阻断 / 参数 / 业务），退出码符合 §8
- [ ] `apiHash` 通过 §2.4 三条官方向量断言（Go 与 Python 结果一致）
- [ ] `verify` 在可用网络下拉到全量成绩，字段含 `comboStatus` 与 `syncStatus`
- [ ] 同步到水鱼后**原有 FC/FS 不被清空**（用含 FC 标记的账号验证）
- [ ] 异常路径（拉取中途失败）仍执行登出，不残留会话
- [ ] `go test ./... -race` 全绿；单测不访问真实网络
- [ ] 日志中不出现二维码/token 全文（人工抽查）
- [ ] **扩展点验收**：新增一个假查分器实现，只需新增 1 个文件 + 1 行注册，既有文件零改动
- [ ] README 能让新接手者独立跑通 `probe` 与 `verify`

---

## 10. 里程碑

| 阶段 | 交付 | 完成判据 |
| --- | --- | --- |
| **M0 算法验证** | `crypto.go` + 表驱动测试 | ✅ **已完成**：Go 与 Python 双实现均通过 1.50 官方向量（§2.4） |
| M1 协议打通 | `protocol` 全包 + `probe`/`verify` CLI | 可用网络下 `verify` 打印出成绩字段结构（含 comboStatus/syncStatus） |
| M2 同步可用 | `sync/divingfish`（含合并）+ 单测 | 同步后水鱼 FC/FS 保留、成绩更新 |
| M3 工程化 | 错误体系、日志脱敏、限频、README、文档 | 满足 §9 全部验收项 |
| M4 扩展 | 落雪、b50、HTTP 服务模式 | 按需 |

> **M1 状态**：`probe` 已通过（标题服务器 Ping + AimeDB 两项均 OK，退出码 0）；剩余为**登录与拉成绩的复验**，需现场扫码（二维码 10 分钟有效）。

---

## 11. README 要求（面向人类，写详细）

README 是本项目「人类接管」的第一入口，**必须包含以下内容且保持更新**：

| 章节 | 必须写什么 |
| --- | --- |
| 一句话简介 | 这个工具做什么、给谁用（处理舞萌 DX 二维码，拉完整成绩并同步到查分器） |
| 状态与限制 | 当前支持到哪个游戏版本、**已知限制**（IP 阻断风险、无官方授权、只支持 1.53+ 参数） |
| 快速开始 | 3 步内跑起来：`make build` → `probe` → `verify`（含二维码从哪来） |
| 命令参考 | 每个子命令：用途、参数、示例、退出码、输出示例（脱敏后的真实输出） |
| **故障排查** | 按现象索引：空响应=IP 阻断怎么办、解密失败=换版本、业务错误码对照表 |
| 架构概览 | 一张依赖方向图 + 每个包的职责一句话（与 §5.2 一致） |
| **常见任务索引** | 「我要做 X」→「改哪个文件」：加版本、加查分器、加 CLI 命令、加 API（对应 §5.3） |
| 安全须知 | 二维码与 token 的敏感性、不要把凭证贴进 issue / 日志 |
| 开发 | 如何跑测试、如何交叉编译、代码风格（引用 §6） |
| 许可与声明 | 风险声明（账号可能被限制、IP 可能被封、无官方授权） |

**写法要求**：
- 面向**第一次拿到这个仓库的人**：假设对方完全不了解背景
- 命令示例必须**可直接复制粘贴**且**有预期输出**
- 每条「常见任务」必须指明**具体文件路径**，不要只说「改协议层」
- 不写无关的通用模板内容（大段贡献者协议等）

---

## 12. 扩展指南（How to ...）

> 每项都遵循 §5.3：**只新增文件 + 注册一行，既有文件不动**。

### 12.1 新增一个协议版本（如 1.56 上线后）

1. 取得新版本的 4 个参数（Encoding、ObfuscateParam、AES Key、AES IV）
2. `internal/protocol/version.go` 的 `Versions` 表加一项
3. `internal/protocol/crypto_test.go` 加该版本的 hash 对照值
4. `mai-arcade probe --version 1.56` 验证连通

### 12.2 新增一个查分器

1. 新增 `internal/sync/newsite.go`
2. 实现 `Syncer` 接口（`Name()` + `Upload()`）
3. `init()` 中 `Register(&NewSite{})`
4. 若该站会「清空缺失字段」（像水鱼），在实现内部完成合并；`service` 层无需知道
5. CLI 参数加对应 flag（或统一用 `--site newsite --credential xxx`）
6. 单测用 `httptest.Server` 覆盖合并与错误分支

### 12.3 新增一个机台 API

1. `internal/protocol/api.go` 加 API 名常量（名字必须与官方变体名逐字符一致，否则 hash 不对）
2. 定义请求/响应结构体（字段名照抄官方，含拼写错误如 `acsessCode`）
3. 在 `title.go` 加方法，内部复用 `call()`
4. 若需登录态，在 `service` 里按「login → call → logout」编排
5. 表驱动测试加一行

### 12.4 新增一个 CLI 子命令

1. 新增 `cmd/mai-arcade/<name>.go`，导出 `run<Name>(ctx, args) error`
2. `main.go` 的 `commands` map 加一行
3. README 的命令参考与常见任务索引补一条

### 12.5 换成 HTTP 服务形态

1. 新增 `cmd/mai-arcade-server/main.go`，只做路由与鉴权
2. 复用 `internal/service` 全部逻辑（**不改动**）
3. 补鉴权（Bearer）+ 限频（令牌桶）+ 请求体大小限制

---

## 附录 A：错误码速查

| 来源 | 码 | 含义 | 建议提示 |
| --- | --- | --- | --- |
| AimeDB | 0 | 成功 | — |
| AimeDB | 1 / 2 | 二维码过期 | 重新从公众号获取 |
| AimeDB | 50 | 签名错误 | 检查 chipID / 时间戳时区 |
| 本地 | 格式非法 | SGID 不合法 | 检查是否复制完整 |
| TitleServer | 1 | 成功 | — |
| TitleServer | 100 | 已登录 | 等待会话释放或先登出 |
| TitleServer | 102 | 二维码过期 | 重新获取 |
| TitleServer | 110 | KeyChip 不匹配 | 机台参数问题 |
| WaterFish | 400 / 500 | `导入token有误` / 认证失败 | Token 无效，去官网重新生成 |
| 网络 | HTTP 200 + 空 body | **版本不被接受**（首选） | 换 `--version` 逐个试（当前用 1.55） |
| 网络 | 所有版本都 200 + 空 body | 再考虑 IP 阻断 / 过频封禁 | 换 IP、或停机十几分钟 |
| 网络 | HTTP 404 + HTML | hash 与版本不配套 | 三者确保同版本 |
| TitleServer | HTTP 500 + HTML | 登录字段形状与版本不符 | 按 §2.5 的版本参数表取字段 |

## 附录 B：参考资料

| 主题 | 出处 |
| --- | --- |
| 协议实现（Rust，1.53 时代，含各版本参数） | `git.fragrance.moe/mokurin000/sdgb-utils-rs` |
| 协议实现（Python） | `git.fragrance.moe/mokurin000/maimaiDX-Api` |
| 完整官方链路（Python，含 login/logout） | `github.com/XiaoLan9999/astrbot_plugin_maimai_updater`（`official_protocol.py`，MAI_ENCODING=1.55） |
| 1.53 参数与哈希 | `git.fragrance.moe/Fragrance/eaquira`（`sdgb/encrypt.py`） |
| 水鱼查分器 API | `maimai.diving-fish.com/manual/docs/developer/zh-api-document` |
| 落雪查分器 API | `maimai.lxns.net/docs/api/maimai` |
| 原理与风险分析 | `xice.cx/posts/toolsForMaimai/`、`xice.cx/posts/153/`、`xice.cx/posts/maiBanIP/` |
| IP 阻断特征 | `github.com/TrueRou/maimai.py/issues/21` |

## 附录 C：已产出的验证代码

| 文件 | 说明 |
| --- | --- |
| `mai_protocol_probe_tmp.py` | Python 探路脚本（`--selftest` 哈希校验 / `--qrcode` 全链路 / `--profile` 版本切换）。依赖 `pycryptodome`（以 `--target` 临时安装，未污染主环境） |
| Go 验证程序（`/tmp/gohash/main.go`） | 一次性验证 `apiHash`：通过 1.50 三条官方向量，1.53/1.55 结果与 Python 逐条一致 |

**已验证结论汇总**：

| 项 | 状态 |
| --- | --- |
| `api_hash` 算法（Go + Python 双实现） | ✅ 通过 1.50 官方向量 |
| AimeDB 连通性与请求格式 | ✅ 实测通过（返回业务错误码，说明格式被接受） |
| 标题服务器连通性 | ✅ **已打通**：`1.55 参数 + Mai-Encoding: 1.55` → `Ping` 返回 `{"result":"Pong"}`（此前记的「IP 被阻断」系版本不被接受导致的误判） |
| 登录 + 拉全量成绩 | ⏳ 用 1.53 字段形状曾得 `HTTP 500`；已按 §2.5 改为 1.55 形状，**待复验** |

## 附录 D：部署前自检命令

```bash
# 1. 确认出口 IP 类型（家宽 or 云服务器）
curl --noproxy '*' -s https://myip.ipip.net

# 2. 算法自检（无需网络）
PYTHONPATH=/tmp/probe_libs python3 mai_protocol_probe_tmp.py --selftest

# 3. 标题服务器连通性（关键：空响应 = 被阻断）
curl --noproxy '*' -s -o /dev/null -w 'HTTP %{http_code} | %{size_download} bytes | %{time_total}s\n' \
  --max-time 25 -X POST "https://maimai-gm.wahlap.com:42081/Maimai2Servlet/Ping" \
  -H "Mai-Encoding: 1.53" -H "Content-Type: application/json" --data-binary '{}'
```
