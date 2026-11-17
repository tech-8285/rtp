# VP9 可伸缩视频的层依赖分析

本报告基于 `github.com/pion/rtp` 当前工作树（分支 `task/45`，提交 `93d6eb3`）的实际源码与测试，回答三个问题：参考图像如何被识别、信息缺失时哪些结论仍有源码依据、以及"片段首尾标记"与"完整图像可解码"之间的距离。

规范依据为 RFC 9628（RTP Payload Format for VP9 Video，即 README 中引用的 `draft-ietf-payload-vp9`，`README.md:37`）。分析时从 <https://www.rfc-editor.org/rfc/rfc9628.txt> 取了一份文本副本到 `/tmp/rfc9628.txt`（仓库外，未提交），下文 `rfc9628.txt:NNN` 的行号均指该副本。

---

## 0. 结论摘要

1. **两个方向的实现并不对称。** 接收端（`VP9Packet`）能解析灵活/非灵活两种模式的全部描述字段；发送端（`VP9Payloader`）只生成极少数子集：灵活模式只写 `I/F/B/E`+PID，**从不写 `P`、`L`、`V`、`P_DIFF`**（`codecs/vp9_packet.go:88-108`）；非灵活模式写 `I/P/B/E/V/Z` 和一个**常量化的 SS 块**，**从不写 `L`**（`codecs/vp9_packet.go:143-206`）。也就是说，灵活模式的参考图像机制（P_DIFF）在发送侧不存在，非灵活模式的分层索引（TID/SID/D/TL0PICIDX）在发送侧也不存在。
2. **接收端能"读到"依赖，但不会"推导"依赖。** `VP9Packet` 只做字段搬运：没有任何把 `P_DIFF` 换算成绝对 PID、把 PID 映射到 PG 下标、或做 2^15/2^7 回绕算术的辅助函数（全文件仅 `Unmarshal`/`parse*`/`IsPartitionHead`，`codecs/vp9_packet.go:253-527`）。"哪些图像可解码"的决定完全在上层。
3. **PID 回绕只实现了一半。** 发送端按 15 bit 掩码并在 `0x8000` 归零（`codecs/vp9_packet.go:39,50-53`，测试 `TestVP9Payloader_Payload/PictureIDOverflow`）；接收端保存 15 bit 数值（`codecs/vp9_packet.go:315-331`），但**不保留 M 位**，因此无法知道该用 7 bit 还是 15 bit 取模——而 RFC 明确要求 P_DIFF 的取模宽度随 PID 宽度变化（rfc9628.txt:495-500）。
4. **空间层依赖只能靠 `D`/`Z` 两个布尔位表达**，二者都被完整解析（`codecs/vp9_packet.go:358-361`），但发送端 `D` 恒为 0（从不写 L）、`Z` 在非灵活模式下恒为 1（`codecs/vp9_packet.go:159`）。`Z` 是常量而非由码流推导——它没有读取 `header` 的任何参考帧信息。
5. **时间层切换的 `U` 位语义在源码中未被使用**，只在 SS 的 PG 表项里被硬编码为 1（`codecs/vp9_packet.go:195`）。RFC 中 `U` 是"时间层切换点"（rfc9628.txt:460-472），不是空间层切换点；空间层的上行切换只能由 `P=0`、`D=0`、`Z` 联合推断。
6. **`B/E` 与 RTP marker 都不等于"完整图像可解码"。** `IsPartitionTail` 直接返回 marker（`codecs/common.go:30-32`），完全忽略 `E` 位；而 marker 在本仓库由 `Packetizer` 按"每次 `Payload()` 调用的最后一个分片"置位（`packetizer.go:132`），即**每帧**一个 marker，而非 RFC 要求的"每个 picture 最高空间层帧的最后一个包"（rfc9628.txt:244-251）。单层流中二者恰好等价，所以现有测试无法暴露差异。

---

## 1. 证据与方法

| 证据类型 | 具体内容 |
|---|---|
| 源码 | `codecs/vp9_packet.go`、`codecs/vp9/header.go`、`codecs/vp9/bits.go`、`codecs/common.go`、`packetizer.go` |
| 已有测试 | `codecs/vp9_packet_test.go`、`codecs/vp9/header_test.go`、`codecs/common_test.go` |
| 规范 | RFC 9628 本地副本 `/tmp/rfc9628.txt` |
| 运行验证 | `go test ./...` 全绿（`codecs`、`codecs/vp9` 等 6 个包 ok） |
| 行为探针 | 独立目录 `/tmp/vp9probe2`，仅通过公开 API（`codecs.VP9Payloader`、`codecs.VP9Packet`、`rtp.NewPacketizer`）调用，`GOPROXY=off` 离线编译 |

下文凡标注"**实测**"的结论，均由该探针在本机复现；探针源码见附录 A。未使用容器，探针目录与仓库分离。

---

## 2. 参考图像识别的三个维度

### 2.1 图像编号（PID）与回绕

发送端：

- 首次初始化时 `pictureID = InitialPictureIDFn() & 0x7FFF`，未提供时随机取 `[0, 0x7FFF)`（`codecs/vp9_packet.go:33-41`）。
- 每次 `Payload()` 调用后自增；`>= 0x8000` 归零（`codecs/vp9_packet.go:50-53`）。因此发送端永远是 15 bit 语义、永远带 `M=1`（`codecs/vp9_packet.go:100-101`）。
- **PID 的计数单位是"一次 `Payload()` 调用"，不是 RFC 意义的 picture。** 代码里没有 picture/空间层的概念，也没有上层传入 SID 的入口（`VP9Payloader` 结构体只有 `FlexibleMode` 和 `InitialPictureIDFn`，`codecs/vp9_packet.go:15-24`）。RFC 要求同一 picture 的所有帧共享 PID（rfc9628.txt:426-434），而 Pion 会在逐层调用时逐层自增——**实测**：连续两次 `Payload()` 同一帧数据得到 `pid=50`、`pid=51`。
- 分片失败（MTU 过小）时仍会消耗一个 PID：`Payload` 先分片再自增，`payloadFlexible`/`payloadNonFlexible` 返回空切片不影响自增。**实测**：先以 MTU=2 调用（返回 0 个分片），再以 MTU=1200 调用，PID 已跳过一号（10→11）。对上层表现为"没有包发出，但图像编号出现空洞"。

接收端：

- `parsePictureID` 读出 7 或 15 bit（`codecs/vp9_packet.go:315-331`），但 `VP9Packet` 结构体**没有字段记录 M 位或位宽**（`codecs/vp9_packet.go:224` 只有 `PictureID uint16`）。**实测**：`0x80 0x81 0x7F`（15 bit=383）与 `0x80 0x7F`（7 bit=127）解析后只留下数值，无法区分二者，也无法做"从 7 bit 切到 15 bit 时零扩展"的换算（rfc9628.txt:415-417）。
- 接收端没有任何回绕比较函数。上层若自行写 `pid_a > pid_b`，在 `0x7FFF→0` 处必然判错；源码中不存在可供复用的正确答案。

结论：**PID 回绕的可识别性，发送端有（15 bit 定宽 + 归零 + 测试），接收端只有"数值"，没有"宽度"和"环绕关系"。**

### 2.2 灵活模式（F=1）

发送端实际生成（`payloadFlexible`，`codecs/vp9_packet.go:58-111`）：

| 字段 | 是否写入 | 源码 |
|---|---|---|
| `I=1`、`F=1` | 恒写（`out[0]=0x90`） | :92 |
| `B` / `E` | 首/末分片置位 | :93-98 |
| PID（15 bit，`M=1`） | 恒写 | :100-101 |
| `P` | **从不写**（恒 0） | —（无 `0x40`） |
| `L`（TID/U/SID/D） | **从不写**（恒 0） | — |
| `P_DIFF`/`N` | **从不写**（因为 P=0，接收端也不会解析） | :287-292 |
| `V`（SS） | **从不写** | — |
| `Z` | **从不写**（恒 0） | — |

这一点与规范直接冲突：RFC 规定灵活模式下 P=1 时"至少一个参考索引 MUST 存在"（rfc9628.txt:488-493）；而 Pion 的灵活模式输出对所有帧（含真正的帧间帧）都声明 `P=0`，即"不使用帧间预测，可以从直接下层空间层上行切换"（rfc9628.txt:335-344）。`payloadFlexible` 根本不读取载荷里的 VP9 帧头（对比非灵活模式 `codecs/vp9_packet.go:133-137` 会调用 `vp9.Header.Unmarshal`），所以它对帧类型一无所知。**一个真正的帧间帧因此被声明成可任意切换的帧**；遵循规范的对端若据此做上行切换，会用到未正确建立预测的参考。这是从源码与实测可以直接读出的行为差异，而非需外部复现的推测。

接收端对灵活模式的解析（`Unmarshal`，`codecs/vp9_packet.go:253-304`）：

- 解析顺序严格按 `I → L → (F&&P 时) P_DIFF → V`（:273-299），与 RFC 图 2 一致。
- `P_DIFF` 最多 3 个（`maxVP9RefPics=3`，:28、:401-418），超过返回 `errTooManyPDiff`（测试 `FlexiblePictureIDRefIndex_TooManyPDiff`，`codecs/vp9_packet_test.go:102-105`）。
- **不校验 `P_DIFF == 0`**。RFC：值为 0 无效（rfc9628.txt:499-501）。**实测**：`P_DIFF=0` 被静默接受并计入 `PDiff`。
- **不校验 `F=1` 必须伴随 `I=1`**。RFC 规定 `I=0` 时 F MUST 为 0 且被接收端忽略（rfc9628.txt:354-362）。**实测**：`0x50`（F=1,I=0,P=1）+ 两个字节被解析成 `PDiff=[1,85]`、payload 为空——即把载荷首字节当成了参考索引，这是对非法组合的"乐观解析"。
- `PDiff` 切片在多包复用时不重置（`append` 无清零）。**实测**：同一 `VP9Packet` 连续解析两个 `P=1,F=1` 包，得到 `[1 2 2]`（上一包的 `[1 2]` 残留）。仓库自己的复用测试用调用方 helper `vp9TrimForReuse` 规避（`codecs/vp9_packet_test.go:237-246`），说明这是**调用方契约**而非 `Unmarshal` 保证。

### 2.3 非灵活模式（F=0）

发送端实际生成（`payloadNonFlexible`，`codecs/vp9_packet.go:113-209`）：

- 先用 `codecs/vp9.Header.Unmarshal(payload)` 解析**码流帧头**（:133-137），只取两个事实：是否关键帧（`NonKeyFrame`）与宽高（`Width()`/`Height()`）。解析失败则整帧丢弃，返回空切片（:134-137）。
- 首字节恒为 `0x80|0x01`（`I=1`、**`Z=1`**，:159）。
- `P = header.NonKeyFrame`（:161-163）。
- `B`/`E` 按分片位置置位（:164-169）。
- 关键帧首分片额外写 `V=1` 与 8 字节 SS（`headerSize = 3+8`，:145-149、:175-199）。

那个 SS 块是**常量**，不是从码流或调用参数推导的：

```
out[off] = 0x10 | 0x08   // N_S=0（1 个空间层）, Y=1, G=1
out[off] = width>>8 ...  // 来自码流帧头
out[off] = 0x01          // N_G=1
out[off] = 1<<4 | 1<<2   // TID=0, U=1, R=1
out[off] = 0x01          // P_DIFF=1
```

即：永远声明"1 个空间层、1 个 picture 的 PG、该 picture 时间层为 0、是切换点、参考相对偏移为 1"。**实测**（关键帧非灵活输出）：

```
hdr=8f I=true P=false L=false F=false B=true E=true V=true Z=true pid=1
ns=0 y=true g=true ng=1 w=[1920] h=[804] pgtid=[0] pgu=[true] pgpdiff=[[1]]
```

其中 `Z=1`、`P_DIFF=1` 与"关键帧不使用帧间预测"语义不自洽：关键帧的 `P=0`，却让 PG 表项声明它参考 `PID-1`。按 RFC，PG 在非灵活模式下用 PID 取模 N_G 索引（rfc9628.txt:419-424）；N_G=1 时所有包都映射到 PG[0]，即**每个包（含关键帧）都带着同一个"参考 PID-1"的表项**。严格按 RFC 解读，`P=0` 的包不解释参考索引（rfc9628.txt:488-493），因此该表项对关键帧是死数据；但它对"PG 描述与码流一致"这一规范性期望（rfc9628.txt:567-570）而言是错的，且接收端无法从描述符本身判断这一项应被忽略。

接收端对非灵活模式的解析：

- `L=1` 时先读公共层字节，再读 `TL0PICIDX`（`codecs/vp9_packet.go:333-344`、:381-390），与 RFC 图 3 一致；`L=0` 时两者都不存在。
- **`TL0PICIDX` 只是被保存**，没有与 `TID`、PID 的任何交叉校验（RFC 要求"PID 映射到 PG 时 TID MUST 等于 PG 表项的 TID"，rfc9628.txt:455-459）。
- `SID` 3 bit 取值范围 0-7，但代码用 `maxSpatialLayers=5` 拒绝 `SID>=5`（`codecs/vp9_packet.go:27,363-365`）。**实测**：`SID=4` 通过，`SID=5/6/7` 返回 `errTooManySpatialLayers`。与此同时 `parseSSData` 的 `NS` 是 3 bit，最多可表示 `N_S+1=8` 个空间层（:446-451），**实测** `NS=6` 被正常接受。同一文件里"SS 支持 8 层"与"SID 最多 5 层"并存，没有注释解释这个 5 的来历。
- PG 循环：`R = (packet[pos]>>2) & 0x3`，即 2 bit（:485），与"最多 3 个参考"一致；`PGTID`/`PGU` 用普通 `append`，`PGPDiff` 用"cap 内复用"（:483-503）。**不重置**。**实测**：先解析 `N_G=3` 的包，再解析 `N_G=1` 的包（不清空切片），得到 `pgtid=[0 1 2 0]`、`pgu=[true false false true]`、`pgpdiff=[[1]]`——**下标 1、2 是上一包的残值**。同样，`Width`/`Height` 在 `Y=0` 的新包后仍保留旧值（协议本身也只在 `Y=1` 时才承诺分辨率，但结构体不区分"没有信息"与"旧信息"）。
  仓库的 `TestVP9Packet_Unmarshal_ReusedPacket` 明确把"清零"交给调用方 helper，并断言 `TestVP9Packet_Unmarshal_ReusedPacketDoesNotAllocate`（`codecs/vp9_packet_test.go:250-273`）。这证明复用契约存在，但也说明**直接连续 `Unmarshal` 的读者会读到脏数据**。

### 2.4 空间层依赖

源码能表达的只有两个布尔位：

- `D`：当前 SID 帧是否依赖同一 picture 的 SID-1 帧（`codecs/vp9_packet.go:361`；RFC rfc9628.txt:473-478，且规定 SID=0 时 D MUST 为 0）。
- `Z`：当前帧是否被更高空间层引用（`codecs/vp9_packet.go:268`；RFC rfc9628.txt:386-393）。

发送端：`D` 恒 0（不写 L），`Z` 在非灵活模式恒 1。**实测**：非灵活模式的帧间包 `P=true Z=true L=false V=false`；灵活模式的同类包 `P=false Z=false L=false`。所以：

- Pion 发出的流**没有空间层信息**，`D`/`Z` 都不承载真实依赖；
- `Z=1` 是"任何更高层都可以丢弃本帧"的断言，却没有检查码流是否真的没有更高层引用它。Pion 自产的流只有一层，断言恰好成立；但该位是常量而非推导值，一旦这条流被网关与其他层合并、或码流实际存在上层引用，按 `Z` 丢弃就会破坏参考链。

接收端能解释的是：`SID`、`D`、`Z`、`NS`、`Width/Height`（按层）。**不能解释的是**：层与层之间的实际参考关系（D 只在 SID-1 方向给出一位）、每层的关键帧周期、以及 `D=1` 时"只有一个直接下层"这一简化假设是否成立。后两者在 RFC 中是显式的简化约定（rfc9628.txt:154-167），源码没有对此做任何校验或注释。

### 2.5 时间层切换

- `TID` 3 bit（:358），`U` 1 bit（:359）。RFC 的 `U` 是**时间层**切换点：当前 picture 的 TID=T 时，后续 TID>T 的图像不会依赖当前图像之前的 TID>T 图像（rfc9628.txt:460-472）。
- 发送端**从不写 L，因此从不写 TID/U/D**；唯一出现 `U=1` 的地方是 SS 里那条常量 PG 表项（:195）。
- 因此 Pion 发送侧不存在时间层调度：`TID` 恒为"不存在"，SS 里恒为 0。实际的时间分层完全由上层是否按 `Payload()` 调用顺序组织决定，RTP 层不参与。
- 接收端可以读 `TID`/`U`/`TL0PICIDX`，但同样没有"根据 U 判断能否丢帧"的逻辑，也没有把 `TL0PICIDX` 关联到具体 base-layer PID 的辅助函数。

### 2.6 发送端生成 vs 接收端可解释：一张对照表

| 信息 | 发送端（Pion）生成 | 接收端（Pion）可解释 | 差距 |
|---|---|---|---|
| PID 存在/数值 | 总是 15 bit | 是（数值） | 位宽 M 未保留 |
| PID 回绕 | 15 bit 归零 | 无算术工具 | 上层易错 |
| 模式 F | 可选，随时可切 | 逐包解析 | 无"仅关键帧可切"校验 |
| `P` | 非灵活=码流帧类型；灵活=恒 0 | 是 | 灵活模式语义错误 |
| `L`（TID/U/SID/D） | 从不生成 | 是 | 发送侧无分层 |
| `TL0PICIDX` | 从不生成 | 是 | 发送侧无时间层索引 |
| `P_DIFF`（灵活） | 从不生成 | 是 | **灵活模式参考机制缺失** |
| PG/SS（非灵活） | 仅关键帧，常量块 | 是（含 PG 数组） | 常量化、不随码流 |
| 分辨率 SS | 关键帧取码流帧头 | 是 | `show_existing_frame` 时为 0 |
| `Z` | 非灵活恒 1 | 是 | 非推导值 |
| B/E | 按分片 | 是（字段） | `IsPartitionTail` 忽略 E |
| marker | 每次 `Payload()` 末片 | `IsPartitionTail` 直接返回它 | 与 picture 语义不一致 |
| 可解码性 | — | **无** | 完全上层职责 |

---

## 3. 参考描述缺失或层信息不完整时，哪些依赖仍有源码依据

### 3.1 仍有源码依据的依赖

以下结论可以在**没有** `L`、`P_DIFF`、SS 的情况下，仅凭本仓库代码得到：

1. **"是否使用帧间预测"**：`P` 位（`codecs/vp9_packet.go:262`）。`P=0` 时按 RFC 可以不做帧间参考（rfc9628.txt:335-344）。但注意 Pion 灵活模式恒置 0，此时该依据**不可信**。
2. **"是否被更高空间层引用"**：`Z` 位（:268）。同样在 Pion 发送端是常量 1，只能当"发送方断言"，不能当码流事实。
3. **"帧边界"**：`B`/`E`（:265-266）。这是分片级事实，源码与测试都覆盖（`TestVP9IsPartitionHead`，`codecs/vp9_packet_test.go:450-465`）。
4. **"是否同一 picture / 是否同一帧的多个分片"**：可通过 (SSRC, timestamp) 与 PID 判断。但**本仓库不提供**这一判断，`VP9Packet` 不含 RTP 头，调用方需自行组合 `rtp.Packet`。
5. **"空间层编号与分辨率"**：仅在 `L=1`/`V=1` 时；`SID`、`Width/SID`、`Height/SID` 都是直接字段。
6. **"同一 picture 内 SID-1 → SID 的依赖"**：`D=1`。`D=0` 表示**不依赖直接下层**，但按 RFC 仍可能依赖本层更早图像。
7. **"时间层切换点"**：`U=1`（若 L 存在）。
8. **"非灵活模式的周期性参考模式"**：`V=1` 且 `N_G>0` 时，PG 表项是规范性声明；接收端拿到的是 `PGTID/PGU/PGPDiff` 数组。

### 3.2 没有源码依据、属于上层职责的决定

| 决定 | 为什么源码给不出 | 依据 |
|---|---|---|
| 当前 PID 对应 PG 的哪一项 | 代码只保存 `PictureID`，无 `PID mod N_G` 计算 | `codecs/vp9_packet.go:478-503` 仅原样存数组 |
| `P_DIFF` → 绝对参考 PID | 无减法、无模运算、无位宽 | `codecs/vp9_packet.go:401-418` 只做 `packet[pos]>>1` |
| 参考帧是否已收到/已解码 | 无参考帧缓存，无丢包跟踪 | 全包无状态机 |
| 能否从 SID=k 切到 SID=k+1 | 需要 `P/D/Z` + 已解码参考集合 | 无决策函数 |
| 时间层能否丢帧 | 需要 `U` + `TID` + 解码状态 | 同上 |
| 同一 picture 的所有帧是否齐全 | `E` 只标帧尾；picture 级需聚合多帧 | `IsPartitionTail` 只看 marker |
| 何时允许切换灵活/非灵活模式 | RFC 限"关键帧首包且 TID=0"；`FlexibleMode` 是普通导出字段，随时可改 | `codecs/vp9_packet.go:17`；**实测**连续两次调用切模式不报错 |
| 码流内部一致性（TID 与 PG 表项是否匹配等） | 需要解析码流帧头并与 RTP 描述比对 | `codecs/vp9` 包有帧头解析，但 `VP9Packet` 不调用它 |
| LRR/FIR/RPSI 反馈 | 仓库无 RTCP 包，无相关类型 | `grep -i rpsi/fir/lrr` 无命中 |

特别地，**码流帧头提供的信息比 RTP 层更丰富，但两者在代码中没有任何连接**：`codecs/vp9.Header` 暴露 `NonKeyFrame`、`ShowFrame`、`ShowExistingFrame`、`ErrorResilientMode`、`FrameSize`（`codecs/vp9/header.go:110-119`），发送端只用了 `NonKeyFrame` 与宽高（`codecs/vp9_packet.go:133-137,145-199`），接收端 `VP9Packet.Unmarshal` 只返回载荷字节，从不解析它（`codecs/vp9_packet.go:301-303`）。

---

## 4. 两种模式的互操作条件

### 4.1 发送侧成立条件（源码可验证）

| 条件 | 源码现状 | 证据 |
|---|---|---|
| 同一 `Payload()` 调用只承载一个 picture 的一层 | **不成立**：无 SID 入参，调用方无法表达"这是同 picture 的 SID=1" | `VP9Payloader` 结构体 :15-24 |
| 同 picture 所有帧共享 PID | **不成立**：每次调用自增 | :50-53；**实测** pid=50→51 |
| 灵活模式下 P=1 必带 P_DIFF | **不成立**：P 恒 0 | :88-108 |
| 模式切换只发生在关键 picture 首包 | **不成立**：`FlexibleMode` 是导出字段，无守卫 | :17；**实测**可任意切换 |
| 关键帧 SS 的 PG 与码流一致 | **不成立**：常量 `TID=0,U=1,P_DIFF=1`，且与 `P=0` 冲突 | :192-198 |
| `Z` 反映真实"不被上层引用" | **不成立**：非灵活模式恒 1 | :159 |
| 关键帧判定与码流一致 | **部分成立**：`NonKeyFrame` 来自帧头；但 `show_existing_frame` 被当作关键帧（`Header.Unmarshal` 提前返回，`NonKeyFrame=false`） | `codecs/vp9/header.go:147-161`；**实测**其输出 `V=1`、`P=0`、`W/H=0`、`P_DIFF=1` |

**实测片段（`show_existing_frame` 输入）**：码流帧头识别为 `ShowExistingFrame=true`、`NonKeyFrame=false`；非灵活 payloader 输出 `P=false V=true w=[0] h=[0] pgtid=[0] pgpdiff=[[1]]`。`show_existing_frame` 只是显示已有参考缓冲，不是关键帧，却获得了关键帧才有的 SS 声明。

### 4.2 接收侧成立条件

Pion 作为接收端比作为发送端更接近规范：

- 两种模式的描述符都能解析，测试覆盖 `NonFlexible`、`FlexibleLayerIndicePictureID`、`FlexiblePictureIDRefIndex`、SS（含 Y/G/NG）（`codecs/vp9_packet_test.go:27-207`）。
- 严格的长度校验：每个可选字段都先检查 `len(packet) <= pos`，短包返回 `errShortPacket`（如 :316、:354、:382、:403、:442、:470、:479、:495）。
- 边界限制：`PDiff` 3 个（:410）、`SID<5`（:363-365）、`NG` 上限由 `uint8` 决定（:474）、PG 表项内参考数上限由 2 bit 决定（:485）。

但仍存在 5 个会影响互操作的边界：

1. `SID=5..7` 被拒（RFC 允许 3 bit）；
2. `P_DIFF=0` 被接受（RFC 禁止）；
3. `F=1,I=0` 被"乐观解析"，把载荷当参考索引（RFC 要求忽略 F）；
4. PID 位宽不保留，无法做正确模运算；
5. 复用 `VP9Packet` 时切片/分辨率残留，需要调用方自行清空（仓库测试用 `vp9TrimForReuse` 示范，但该 helper 在 `_test.go` 内，不对外导出）。

### 4.3 双向互操作的最小成立集合

- **Pion→外部**：只有当流是**单空间层、单时间层、每帧一次 `Payload()`、且 `FlexibleMode=false`** 时，输出才是自洽的（SS 声明 1 层、N_G=1、PID 连续）。灵活模式输出只能被"忽略 P/L/V"的对端接受；任何按 RFC 检查"P=1 必有 P_DIFF"或依赖 `Z` 的对端都会得到错误结论。
- **外部→Pion**：只要不触发 4.2 的五个边界，Pion 能逐包还原描述字段；但"能否解码"必须由上层另行实现。
- **互操作不成立的条件**（源码层面无补救）：多空间层、真实时间分层、模式切换、以及任何需要"参考图像集合"的判定。

---

## 5. 层切换成立的条件

### 5.1 空间层上行切换

按 RFC，可切换的前提是目标层帧及其依赖链可解码：`P=0`（无帧间预测）或参考都在已解码集合中；`D=0` 或同 picture 的下层已解码；`Z=0` 的下层帧不能被丢弃（rfc9628.txt:335-344, 386-393, 473-478）。

源码能支撑的部分：

- `P`、`D`、`Z`、`SID` 都能读出（:262, :361, :268, :360）。
- `NS`/`Y`/`Width`/`Height` 给出层数与分辨率（:446-467）。

源码不能支撑的部分：

- 没有"参考是否已解码"的状态；
- 没有把 `D=1` 展开为"需要 SID-1 的哪一帧"的逻辑（RFC 只保证 SID-1，同一 picture，仍需调用方按 timestamp 关联）；
- 没有 `PID` 回绕下的参考定位（见 2.1）。

因此**空间层切换在本仓库内不可能"成立"**，只能"提供素材"。

### 5.2 时间层切换

按 RFC，`U=1` 表示后续更高 TID 的图像不会依赖当前图像之前的更高 TID 图像（rfc9628.txt:460-472）；`TL0PICIDX` 给出当前图像依赖的 base-layer 图像（rfc9628.txt:479-487）。

源码现状：

- 接收端可读 `U`/`TID`/`TL0PICIDX`，但无任何使用点（`grep` 仅解析与测试）；
- 发送端从不生成 `L`，`TID` 与 `TL0PICIDX` 在 Pion 发出的流中不存在；
- `TL0PICIDX` 是 `uint8`，回绕语义（255→0，rfc9628.txt:484-487）没有对应处理。

结论：时间层切换同样是**上层职责**；仓库只保证"如果有这些位，就能原样读出来"。

### 5.3 模式切换

RFC 要求只在"关键 picture 首包、TID=0"时切换（rfc9628.txt:167-173）。`VP9Payloader` 对此没有任何检查：`Payload()` 每次都直接按当次 `FlexibleMode` 分支（`codecs/vp9_packet.go:43-48`）。**实测**：同一 payloader 先 `FlexibleMode=true` 再改 `false`，两次输出分别 `F=1`/`F=0`，无错误、无告警。接收端同样无跨包状态，不会察觉模式变了。

---

## 6. 片段首尾标记与"完整图像可解码"的证据边界

### 6.1 `B`/`E` 的证据强度

- `B`：`out[0] |= 0x08` 于首个分片（`codecs/vp9_packet.go:93-95`、:164-166）；`IsPartitionHead` 只查 `payload[0]&0x08`（:521-527）。**B 只说明"当前 RTP 包载荷的首字节是一个 VP9 帧的起点"，不说明它是 picture 的首帧**（RFC 明确"this frame might not be the first frame of a picture"，rfc9628.txt:365-370）。
- `E`：末分片置位（:96-98、:167-169）。`VP9Packet.E` 被解析（:266），但**没有任何消费者**。

### 6.2 marker 与 E 的关系

- 接收端 `IsPartitionTail(marker, _)` **直接返回 marker**，忽略载荷与 E（`codecs/common.go:30-32`）。这是全部视频编解码器共用的 mixin（H264/H265/AV1/VP8/VP9 都嵌入它）。
- 发送端 `Packetizer` 把 marker 置为"本次 `Payload()` 返回切片的最后一个"（`packetizer.go:126-140`，其中 :132）。RTP 时间戳只在 `Packetize` 结束后累加（:141）。
- RFC 的 marker 语义是"picture 最高空间层帧的最后一个包"（rfc9628.txt:244-251）；Pion 的 marker 是"本次调用最后一个包"。单层流下等价，多层流下不等价（低层帧的 E=1 但 marker=0）。

**实测**：描述符 `E=1` 时 `IsPartitionTail(marker=false, …) == false`；`E=0` 时 `IsPartitionTail(marker=true, …) == true`。两者完全解耦。

### 6.3 从"最后一个分片到了"到"图像可解码"之间缺什么

即使 `B`、`E`、marker 全部正确，仍不足以断言可解码，源码中缺失的证据链是：

1. **picture 聚合**：同一 timestamp/SSRC 下，SID 0..k 的帧是否都到齐。仓库无帧组装器（`grep` 无 assembler / depacketize 逻辑；`VP9Packet` 只做描述符解包）。
2. **参考链**：`P_DIFF`/PG 指出的参考 PID 是否收到并成功解码。无参考缓存。
3. **空间链**：`D=1` 时同 picture 的 SID-1 帧是否在同层解码前到达。`D` 只是标志。
4. **码流自洽性**：`error_resilient_mode`、`show_frame`、`show_existing_frame` 只能从 `codecs/vp9.Header` 得到，而 RTP 解包路径不解析码流（`codecs/vp9_packet.go:301-303` 直接 `Payload = packet[pos:]`）。
5. **中间盒改写**：RFC 要求"若删除了更高空间层，marker 必须置在被保留的最高层帧上"（rfc9628.txt:248-251）。Pion 没有任何改写/重写 marker 的代码。
6. **时间戳一致性**：RFC 要求同 picture 所有帧同 timestamp（rfc9628.txt:259-265）。`Packetizer` 由调用方通过 `samples` 参数决定是否推进时间戳；`Packetize(inter, 0)` 会让两帧共享时间戳——**实测** `ts equal=true` 且 PID 不同（7、8）。这是合法的"调用方表达同 picture"方式，但仓库不校验 PID 与 timestamp 的对应关系，也没有把"同 timestamp 多 PID"与"同 PID 多 SID"统一起来的视图。

因此，**`E`/marker 能证明的只有"一个 VP9 帧的最后一个分片已到达"**；"一个 picture 可解码"需要上层把 RTP 描述、码流帧头、参考缓存与丢包状态合起来判断，本仓库不提供这些证据。

---

## 7. 已有测试覆盖与尚缺的互操作性证据

### 7.1 已覆盖（源码内可查）

| 测试 | 覆盖内容 | 位置 |
|---|---|---|
| `TestVP9Packet_Unmarshal` | 两种模式的描述符、SS（Y/G/NG/P_DIFF）、短包错误、PDiff 上限 | `codecs/vp9_packet_test.go:13-222` |
| `TestVP9Packet_Unmarshal_ReusedPacket` | 复用同一 `VP9Packet` + 调用方 trim 后与全新解析一致 | :250-263 |
| `TestVP9Packet_Unmarshal_ReusedPacketDoesNotAllocate` | trim 后 SS 解析零分配（切片容量内） | :265-273 |
| `TestVP9Payloader_Payload` | 两模式的分片字节金标准、大小 MTU、关键/非关键帧、SS 常量块 | :275-419 |
| `TestVP9Payloader_Payload/PictureIDOverflow` | PID 在 `0x7FFF` 后归零 | :420-447 |
| `TestVP9IsPartitionHead` | B 位 | :450-465 |
| `TestHeaderUnmarshal` | VP9 帧头：profile 1/2、`show_existing_frame`、分辨率、`color_config` 分支（无 profile 3） | `codecs/vp9/header_test.go:12-160` |
| `TestZeroAllocations` | VP9 描述符（非 SS）零分配 | `codecs/common_test.go:23-244` |

### 7.2 尚缺的互操作性证据（按优先级）

1. **跨实现向量**：所有 VP9 测试字节要么是手写，要么是 Pion 自己 payloader 的输出（`codecs/vp9_packet_test.go:275-419` 是自产自消）。没有任何 libvpx / Chrome / WebRTC 真实抓包。因此"Pion 输出能被遵循规范的对端接受"**未经测试**。
2. **marker 与 E 在多层下的差异**：`IsPartitionTail` 无 VP9 专属测试（H265、AV1 都有，见 `codecs/h265_packet_test.go:382-400`、`codecs/av1_depacketizer_test.go:390-398`）。单层流掩盖了 6.2 的偏差。
3. **`show_existing_frame`**：帧头测试有该用例（`header_test.go:65-79`），但**没有**"payloader 不应把它当关键帧"的测试；探针实测的 `V=1/W=0` 输出无人断言。
4. **`P_DIFF` 的语义测试**：只验证"读出来是什么数"（`:91-100`），没有"PID 与 P_DIFF 组合后的参考 PID"测试，也没有跨 `0x7FFF/0x7F` 回绕的用例。
5. **7/15 bit PID 切换**：无测试；结构体也没有字段可测。
6. **模式切换**：无测试（RFC 只在关键 picture 首包允许）。
7. **PG 索引映射**：无测试。`PictureID mod N_G` 完全未实现。
8. **多空间层发送**：无测试（API 也不支持）。
9. **复用脏数据**：`ReusedPacket` 测试依赖 `_test.go` 里的 `vp9TrimForReuse`，没有"不 trim 时字段残留"的负向测试；探针实测的 `pgtid=[0 1 2 0]`、`Width` 残留无人断言。
10. **零分配的范围**：`TestZeroAllocations` 的 VP9 向量是 `0xA0 0x02 0x23 0x01 0xAA`（I=1、L=1、F=0），不含 SS、P_DIFF、PG（`common_test.go:38-41`）；探针实测含 SS 的**新建**包为 8 allocs/op，而同一对象复用后为 0。且 `VP9Packet` 从不读取 `zeroAllocation` 字段（`grep zeroAllocation codecs/vp9_packet.go` 无命中），`SetZeroAllocation(true)` 对 VP9 的首次解析实际不生效——"零分配"只对调用方预先扩容并自行清空的切片成立。
11. **RTCP 反馈**：RPSI/FIR/LRR 完全不存在，无法验证 RFC 第 5 章相关的层刷新流程（rfc9628.txt:650-723）。
12. **SDP 参数**：本仓库不处理 `profile-id`/`max-fr`/`max-fs`（属 `pion/webrtc`），因此"协商后才可用的可伸缩配置"没有证据。

---

## 8. 结论

- **参考图像识别**：接收端具备"逐包读取规范字段"的能力；发送端只实现了最小可用的单层封装。灵活模式的参考机制（`P`+`P_DIFF`）在发送侧缺失，非灵活模式的分层索引（`L`）在发送侧缺失，两者恰好各缺一半。
- **PID 回绕**：发送侧正确（15 bit、归零、有测试），接收侧只有数值，没有位宽与模运算，回绕算术必须由上层实现且容易出错。
- **空间层依赖**：只能通过 `SID`/`D`/`Z`/`P` 表达；Pion 发送端不生成 `SID`/`D`，`Z` 为常量，因此其输出不携带可用的空间层依赖。
- **时间层切换**：`TID`/`U`/`TL0PICIDX` 可读不可用；发送端不生成。
- **信息缺失时的依据**：帧边界（`B`/`E`）、`P`、`Z`、以及存在时的 `L`/`V` 字段是源码可直接依据的全部；参考解析、PG 映射、可解码性、模式切换许可、层选择均属上层职责，源码无对应实现或状态。
- **互操作与切换成立条件**：Pion 作为发送端仅在"单空间层 + 单时间层 + 非灵活模式 + 每帧一次调用"时自洽；作为接收端在避开第 4.2 节 5 个边界时可解析两种模式描述符，但无法独立判定可解码性。
- **B/E 与可解码性**：`E` 与 marker 在源码中完全解耦（`IsPartitionTail` 只看 marker），且 marker 的置位语义是"每帧"而非"每 picture"；两者都只能证明帧的传输边界，不能证明 picture 完整或可解码。

以上均以当前源码与测试为据；标注"实测"的行为可用附录 A 的公开 API 探针复现。仓库内不存在能证明跨实现互操作性的测试向量，这一点是本报告最大的证据空白。

---

## 附录 A：行为探针（公开 API）

探针位于仓库之外的独立目录，`go.mod` 以 `replace` 指向本仓库，离线运行（`GOPROXY=off`）。

```go
package main

import (
    "fmt"

    "github.com/pion/rtp"
    "github.com/pion/rtp/codecs"
)

func desc(b []byte) codecs.VP9Packet {
    var p codecs.VP9Packet
    p.Unmarshal(b)
    return p
}

func main() {
    key := []byte{0x82, 0x49, 0x83, 0x42, 0x0, 0x77, 0xf0, 0x32, 0x34}
    inter := []byte{0x86, 0x0, 0x40, 0x92, 0xe1, 0x31, 0x42, 0x8c, 0xc0, 0x40}

    // 1) 灵活模式对真实帧间帧仍输出 P=0、无 L/V/Z
    pf := &codecs.VP9Payloader{FlexibleMode: true, InitialPictureIDFn: func() uint16 { return 0 }}
    for _, pl := range [][]byte{key, inter} {
        d := desc(pf.Payload(1200, pl)[0])
        fmt.Printf("flex: P=%v L=%v V=%v Z=%v pid=%d\n", d.P, d.L, d.V, d.Z, d.PictureID)
    }

    // 2) 非灵活模式：Z=1、关键帧 SS 常量块
    pn := &codecs.VP9Payloader{FlexibleMode: false, InitialPictureIDFn: func() uint16 { return 0 }}
    for _, pl := range [][]byte{key, inter} {
        d := desc(pn.Payload(1200, pl)[0])
        fmt.Printf("nonflex: P=%v L=%v V=%v Z=%v pid=%d ns=%d ng=%d w=%v\n",
            d.P, d.L, d.V, d.Z, d.PictureID, d.NS, d.NG, d.Width)
    }

    // 3) show_existing_frame 被当成关键帧
    se := []byte{0b10101010, 0x49, 0x83, 0x42, 0x40, 0xef, 0xf0, 0x86}
    d := desc(pn.Payload(1200, se)[0])
    fmt.Printf("show_existing: P=%v V=%v w=%v h=%v pgtid=%v pgpdiff=%v\n",
        d.P, d.V, d.Width, d.Height, d.PGTID, d.PGPDiff)

    // 4) PID 位宽不可区分
    a := desc([]byte{0x80, 0x81, 0x7F, 0xAA}) // 15 bit
    b := desc([]byte{0x80, 0x7F, 0xAA})       // 7 bit
    fmt.Printf("pid: 15bit=%d 7bit=%d\n", a.PictureID, b.PictureID)

    // 5) PDiff 复用累积
    var g codecs.VP9Packet
    g.Unmarshal([]byte{0xD0, 0x02, 0x03, 0x04, 0xAA})
    g.Unmarshal([]byte{0xD0, 0x03, 0x05, 0x06, 0xAA})
    fmt.Printf("pdiff reuse=%v\n", g.PDiff)

    // 6) L 清零后层信息残留
    var h codecs.VP9Packet
    h.Unmarshal([]byte{0xA0, 0x01, 0x23, 0x05, 0xAA})
    h.Unmarshal([]byte{0x80, 0x02, 0xAA})
    fmt.Printf("after L=0: L=%v TID=%d SID=%d D=%v TL0=%d\n", h.L, h.TID, h.SID, h.D, h.TL0PICIDX)

    // 7) SID 上限 5 vs SS 的 NS 上限 8
    for _, sid := range []uint8{4, 5, 7} {
        var q codecs.VP9Packet
        _, err := q.Unmarshal([]byte{0xB0, 0x01, sid << 1, 0xAA})
        fmt.Printf("SID=%d err=%v\n", sid, err)
    }

    // 8) marker 与 E 解耦
    var v codecs.VP9Packet
    fmt.Println(v.IsPartitionTail(false, []byte{0x94, 0x01, 0x02}), // E=1, marker=false
        v.IsPartitionTail(true, []byte{0x90, 0x01, 0x02}))          // E=0, marker=true

    // 9) marker 由 Packetizer 按“本次调用末片”置位
    pk := rtp.NewPacketizer(1200, 96, 1, pn, rtp.NewFixedSequencer(1), 90000)
    p1 := pk.Packetize(inter, 3000)
    p2 := pk.Packetize(inter, 3000)
    fmt.Println(p1[len(p1)-1].Header.Marker, p2[len(p2)-1].Header.Marker)

    // 10) 非关键 picture 上随时切模式
    pm := &codecs.VP9Payloader{FlexibleMode: true, InitialPictureIDFn: func() uint16 { return 1 }}
    fmt.Printf("mode: F=%v", desc(pm.Payload(1200, key)[0]).F)
    pm.FlexibleMode = false
    fmt.Printf(" -> F=%v", desc(pm.Payload(1200, inter)[0]).F)
    pm.FlexibleMode = true
    fmt.Printf(" -> F=%v\n", desc(pm.Payload(1200, inter)[0]).F)
}
```

实测输出（与上面代码逐行对应）：

```
flex: P=false L=false V=false Z=false pid=0
flex: P=false L=false V=false Z=false pid=1
nonflex: P=false L=false V=true Z=true pid=0 ns=0 ng=1 w=[1920]
nonflex: P=true L=false V=false Z=true pid=1 ns=0 ng=0 w=[]
show_existing: P=false V=true w=[0] h=[0] pgtid=[0] pgpdiff=[[1]]
pid: 15bit=383 7bit=127
pdiff reuse=[1 2 2]
after L=0: L=false TID=1 SID=1 D=true TL0=5
SID=4 err=<nil>
SID=5 err=too many spatial layers
SID=7 err=too many spatial layers
false true
true true
mode: F=true -> F=false -> F=true
```
