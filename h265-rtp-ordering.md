# H265 RTP 聚合与分片中的解码顺序信息保留

分析对象：`codecs/h265_packet.go`（本仓库 `pion/rtp`，commit `93d6eb3`）。
协议基准：RFC 7798（RTP Payload Format for HEVC），`/tmp/rfc7798.txt` 为逐条引用来源。

本文所有结论按证据强度分三级标注：

| 标记 | 含义 |
| --- | --- |
| **[源码契约]** | RFC 规范文本与源码逐行对应，可直接判定；不需要外部证据 |
| **[跨层推断]** | 依赖调用方（`rtp.Packetizer`、`SampleBuilder`、SFU 等）如何使用本库，源码本身不足以定论 |
| **[需互操作证据]** | 需要 SDP 协商、对端实现或抓包才能判定；本仓库内不可判定 |

---

## 0. 摘要

1. **LayerID/TID 在每个封装层都是"随包自带"的 16 位 NAL 头**，`h265SingleNALUnitPacket` 甚至把 RTP PayloadHdr 与 NAL 头复用为同一个 `H265NALUHeader` 字段，因此单 NAL 路径不存在丢失可能。AP 路径为每个 AU 独立解析其自带 NAL 头，AP 自身的 PayloadHdr 只是"全 AU 最小值"的派生元数据，**不参与媒体重建**。FU 路径每片都带完整 LayerID/TID，重建时只取首片。
2. **DONL/DOND 是本库唯一在带内携带解码顺序的字段，但它被"读出即丢弃"**：`H265Depacketizer` 内部会重建每个 AU/NAL 的 DON，随后 `toAnnexB` 在写出媒体前显式把 `donl` 置空（`h265_packet.go:221-230`）。因此该层**不输出任何跨包解码顺序**，只能处理"传输顺序 == 解码顺序"的流。
3. **`hasDonl` 是一个不可从公开 API 设置、不可中途改变的二值开关**，其语义与 `sprop-max-don-diff > 0` 一一对应。两种取值下同一字节串有不同解释，且**存在人为构造的字节级歧义**（同一 15 字节既合法解释为"无 DONL 的 AP"，又合法解释为"带 DONL 的 AP"，且输出媒体不同）。这使 SDP 参数成为唯一的判别依据 —— 属于 **[需互操作证据]**。
4. **接收侧对 DONL 的定位依赖传输顺序启发式**：`parseDonl := len(d.partials) == 0 && d.hasDonl`（`:1620`）。首片丢失、S 片在缓冲非空中到达、或 PACI 包裹的续片到达时，都会**把 DONL 当作载荷/把载荷当作 DONL**，产生静默的媒体字节错位。
5. **发送侧（`H265Payloader`）从不产生 DONL/DOND**（`AddDONL` 已标注 `Deprecated: Has no effect`，`:1742-1744`），内部分组包时 `donl` 恒为 `nil`（`:1849-1853`）。因此本发送端**只对 `sprop-max-don-diff = 0` 的会话是自洽的**。

---

## 1. 字段位置：三层承载结构

### 1.1 NAL 头位布局（`h265_packet.go:51-116`）

```
+---------------+---------------+
|0|1|2|3|4|5|6|7|0|1|2|3|4|5|6|7|
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|F|   Type    |  LayerID  | TID |
+-------------+-----------------+
```

| 字段 | 位 | 访问器 | 语义 |
| --- | --- | --- | --- |
| F | 15 | `F()` `:68` | 禁止位 |
| Type | 14:9（6 bit） | `Type()` `:73` | NAL 单元类型；48/49/50 为 RTP 级包装类型 |
| LayerID | 8:3（6 bit） | `LayerID()` `:89` | 层标识 |
| TID | 2:0（3 bit） | `TID()` `:97` | 时域标识（越小越重要） |

`IsTypeVCLUnit()`（`:81`）判定 `Type & 0b100000 == 0`，即 Type < 32。这是本库区分"可送解码器"与"NAL-unit-like 结构"的唯一判据。

### 1.2 四种封装对三个标识的承载方式

| 封装 | LayerID/TID 来源 | 解码顺序编号承载 | 源码位置 |
| --- | --- | --- | --- |
| Single NAL（Type ≠ 48/49/50） | PayloadHdr 与 NAL 头**同一字段** `payloadHeader` | 条件 DONL（2 字节，紧随头后） | `:137-242` |
| AP（Type=48） | AP 头 = 全 AU 的 **min**；每个 AU 内嵌自己的完整 NAL 头 | 首 AU：DONL(2)；后续 AU：DOND(1) | `:326-587` |
| FU（Type=49） | FU PayloadHdr 保留原 NAL 的 F/LayerID/TID，Type 改写为 49；原 Type 存入 FuType | 仅 S=1 的片带 DONL(2) | `:859-1021` |
| PACI（Type=50） | 复制到 PACI PayloadHdr；内层 NAL 头前 2 字节不上线 | 依 cType 决定内层 DONL 位置 | `:1134-1479` |

**要点 [源码契约]**：`h265SingleNALUnitPacket` 只有 `payloadHeader` 一个字段（`:137-144`），它同时充当 RTP PayloadHdr 和写回 Annex-B 的 NAL 头（`serialize` `:232-242`）。因此 RFC 7798 §4.4.1 的"PayloadHdr SHOULD 是所含 NAL 头的精确副本"在此实现中是**结构性成立**——不存在两份副本，也就无从丢失或分叉。

### 1.3 各封装的 DONL/DOND 字节偏移（`hasDonl = true` 时）

| 封装 | 布局 | 源码 |
| --- | --- | --- |
| Single | `PayloadHdr[0:2] \| DONL[2:4] \| payload[4:]` | `:198-204` |
| AP | `PayloadHdr[0:2] \| DONL[2:4] \| len[2] NAL \| (DOND[1] len[2] NAL)*` | `:385-392` / `:540-578` |
| FU | `PayloadHdr[0:2] \| FUHeader[2] \| DONL[3:5] \| payload[5:]` | `:904-915` |
| PACI→Single | `...PHES \| DONL[0:2] \| payload` | `:1350-1361` |
| PACI→FU | `...PHES \| FUHeader[0] \| DONL[1:3] \| payload` | `:1328-1334` |
| PACI→AP | `...PHES \| DONL[0:2] \| (len NAL)+` | `:1306-1318` |

注意 PACI 内层 FU 的 DONL 处理**不检查 S 位**（见 §6.3）。

---

## 2. 聚合包（AP）：标识合成与顺序编号

### 2.1 发送侧：`newH265AggregationPacket`（`:445-532`）

**LayerID/TID 合成 [源码契约]**

```go
layerID := firstPacket.payloadHeader.LayerID()   // :469
tid     := firstPacket.payloadHeader.TID()       // :470
...
if pLayerID < layerID { layerID = pLayerID }     // :504-506
if pTid < tid { tid = pTid }                     // :507-509
header |= uint16(tid)                            // :518
header |= uint16(layerID) << 3                   // :519
```

与 RFC 7798 §4.4.2 逐字一致：*"The value of LayerId MUST be equal to the lowest value of LayerId of all the aggregated NAL units. The value of TID MUST be the lowest value of TID of all the aggregated NAL units."*

F 位同样按"任一 AU 为 1 则置 1"合成（`:500-502`、`:521-523`），与 §4.4.2 一致。

**DONL→DOND 转换 [源码契约]**

```go
donlExpected := packets[0].donl != nil           // :453  ← 全有或全无
if donlExpected && packet.donl == nil {
    return nil, errMissingDonl                    // :476-478
}
dond := int(*packet.donl) - int(*lastDonl) - 1    // :483
if dond < 0      { return nil, errDonlOutOfOrder } // :484-486
if dond > 255    { return nil, errDondTooLarge }   // :487-489
payload = append(payload, uint8(dond))            // :490
packet.donl = nil                                 // :494 ← AU 内不再重复 DONL
```

- AP 的 `donl` 字段取**首 AU 的 DONL**（`:454-458`），而不是 AP 级 DON。这与 RFC 中"DONL 属于第一个 aggregation unit"的语义一致。
- 编码区间 `dond ∈ [0,255]` ⇒ 相邻 AU 的 DON 差 `∈ [1,256]`，与 RFC 的"DOND 加 1"定义一致。
- **编码器不允许回绕**：`int` 相减得到负值即 `errDonlOutOfOrder`。而 RFC §4.4.2 的解码式是 *"preceding DON plus DOND plus 1 **modulo 65536**"*。本地复现：`DONL=0xFFFE` 后接 `DONL=1` 被判为 out-of-order，而该序列在 RFC 下合法（回绕）。→ **[源码契约：编码与解码的模运算不对称]**，但仅包内可达（无公开复现，见 §9.1）。

**内部构造器的 panic 路径 [仅包内可达]**

`lastDonl := packets[0].donl`（`:474`）。若 `packets[0].donl == nil` 而后续某个 packet 的 `donl != nil`，则 `i>0 && packet.donl != nil` 成立时 `*lastDonl` 解引用 nil。

- 本地复现（包内测试）：`newH265AggregationPacket({donl:nil, donl:&5})` → `invalid memory address or nil pointer dereference`。
- **无公开 API 复现**：`H265Payloader` 构造的所有 AU 的 `donl` 恒为 `nil`（`:1849-1853`），因此该路径从公开入口不可达。按仓库政策（AGENTS.md），此处只报告为"内部不变式缺失"，不宣称公开漏洞。

### 2.2 接收侧：`splitH265AggregationPacket`（`:534-587`）

```go
curDonl := packet.donl                       // :535  ← AP 的 DONL 即首 AU 的 DON
...
if i == 0 {
    donl = curDonl                            // :554-555
} else {
    donlValue := *curDonl + uint16(payload[0]) + 1  // :557  ← 自然模 65536 回绕
    curDonl = &donlValue
    payload = payload[1:]                     // :560
}
parsed, err := parseH265SingleNalUnitPacket(payload[2:2+curLen], false) // :569
...
parsed.donl = donl                            // :574-576
```

关键性质：

1. **每个 AU 用线上自带的 2 字节 NAL 头重建**（`:569` 传入 `withDONL=false`，因为 AU 内不再有 DONL）。因此**每个 AU 的 LayerID/TID 被逐字保留**，与 AP PayloadHdr 无关。
   本地复现：构造 AP PayloadHdr `layer=0/tid=1`，AU1 `layer=1/tid=1`，AU2 `layer=2/tid=1`；解包后得到 `layer=1` 与 `layer=2`，AP 头被完全忽略。
2. **DON 重建使用 `uint16` 加法，天然实现 modulo 65536**（`:557`），与 RFC 一致；这与发送侧的"禁止回绕"构成不对称。
3. **重建出的 DON 不会影响输出顺序**：`handleAggregationUnit`（`:1565-1577`）按 AU 在 AP 中的出现顺序调用 `toAnnexB`，而 `toAnnexB`（`:221-230`）在序列化前把 `donl` 置 `nil`。因此 **DON 被计算出来后立即丢弃**。
4. 由于编码式强制"相邻 DON 差 ≥ 1"，AP 内部重建出的 DON **严格递增**，所以"线上顺序 == AP 内解码顺序"是**构造性不变量**，无需排序即成立。这与 RFC §4.4.2 "NAL units aggregated in one AP are in NAL unit decoding order" 一致。

**由此得到本库最重要的顺序契约 [源码契约]**：
> AP 内部的 DONL/DOND 只能表达**间隙（gap）**，不能表达**置换（permutation）**。因此"按线上顺序输出"与"按 DON 排序输出"在 AP 内等价；跨 AP 的顺序信息在本层完全不存在。

### 2.3 AP 头与 AU 头不一致时不做校验

`parseH265AggregationPacket`（`:365-401`）只校验：

- 长度下限（`:375-377`）
- `header.IsAggregationPacket()`（`:381-383`）
- 有 DONL 时读 `buf[2:4]`（`:387-390`）

它**不校验**：AP PayloadHdr 的 F 位、LayerID/TID 是否等于各 AU 的最小值、AU 数是否 ≥ 2（后者在 `split` 的 `:582-584` 才检查）。

`12/16` 字节构造的"AP 头 layer=7，AU 头 layer=1/2"能被无错解包，媒体保留 1/2。→ 见 §8.1 的语义结论。

---

## 3. 分片包（FU）：标识保留与顺序编号

### 3.1 发送侧：`newH265FragmentationPackets`（`:943-995`）

**Type 替换、其余位保留 [源码契约]**

```go
func newH265FragmentationPacketHeader(payloadHeader H265NALUHeader) H265NALUHeader {
    typeMask := ^uint16(0b01111110_00000000)      // :922
    return H265NALUHeader((uint16(payloadHeader) & typeMask) | (h265NaluFragmentationUnitType << 9))
}
```
掩码只清 Type 位，F/LayerID/TID 原样保留 → 每片都携带被分片 NAL 的完整 LayerID/TID，与 RFC §4.4.3 *"The fields F, LayerId, and TID MUST be equal to ... the fragmented NAL unit"* 一致。

**DONL 只放在首片 [源码契约]**

```go
firstPacket := h265FragmentationPacket{..., donl: packet.donl, ...}  // :965-970
p           := h265FragmentationPacket{..., donl: nil, ...}          // :974-981
lastPacket  := h265FragmentationPacket{..., donl: nil, ...}          // :986-992
```
与 RFC §4.4.3 一致：DONL 仅在 `S=1` 时存在。

### 3.2 接收侧：`rebuildH265FragmentationPackets`（`:997-1021`）

```go
if !packets[0].fuHeader.S() { return nil, errFirstFragmentationUnitMissing }  // :1002-1004
if !packets[len-1].fuHeader.E() { return nil, errLastFragmentationUnitMissing } // :1005-1007
rebuilt := h265SingleNALUnitPacket{
    payloadHeader: rebuildH265FragmentationPacketHeader(packets[0].payloadHeader, packets[0].fuHeader), // :1015
    donl:          packets[0].donl,                                                                     // :1016
    ...
}
```

- 原 NAL Type 从**首片的 FuType** 恢复（`:928-936`）；F/LayerID/TID 从**首片的 FU PayloadHdr** 取。
- **不校验**整串分片的 F/LayerID/TID/FuType 是否一致。本地复现：首片 `layer=1/tid=3/FuType=19`，末片 `layer=2/tid=0/FuType=1`，重建结果静默取 `layer=1/tid=3/type=19`。
- 重建出的 `donl` 随后同样被 `handleSingleUnit → toAnnexB` 丢弃（`:1558-1563`）。

### 3.3 DONL 定位依赖"分片缓冲是否为空"，而不是 S 位

`H265Depacketizer.Unmarshal`（`:1609-1670`）：

```go
case header.IsFragmentationUnit():
    parseDonl := len(d.partials) == 0 && d.hasDonl     // :1620
    fragment, err := parseH265FragmentationPacket(payload, parseDonl)
```

这是本层唯一"看跨包状态"的判断。它是**传输顺序假设**，不是协议字段：它假定"当前缓冲为空 ⇒ 这是某 NAL 的首片 ⇒ 有 DONL"。只要该假设不成立，DONL 与载荷就会错位，见 §6。

---

## 4. 传输顺序 vs 视频解码顺序：责任划分

| 职责 | 承担者 | 依据 |
| --- | --- | --- |
| RTP 序列号、时间戳、M 位 | `packetizer.Packetize`（`packetizer.go:115-141`） | 本文件不读取序列号/时间戳；`IsPartitionTail` 接收 `marker` 参数但仅作回退值 |
| 包到达排序 / 抖动缓冲 | **本库之外**（无实现） | 全仓库 grep 无 `AbsDon` / jitter / reorder 逻辑；`av1_depacketizer.go:39` 明确"caller is responsible for reordering RTP packets" |
| AP 内部解码顺序 | AP 的 DONL + DOND（仅表达间隙） | `:483`、`:557`；线上顺序即解码顺序 |
| 跨包 / 跨 NAL 解码顺序 | **无带内输出** | `toAnnexB` 丢弃 DONL（`:224-227`）；`H265Depacketizer.Unmarshal` 只返回 Annex-B |
| AbsDon 推导（RFC §4.6） | **未实现** | grep 无相关符号 |
| AU 边界 | `IsPartitionHead/IsPartitionTail`（部分）+ 调用方 | `:1672-1700`；见 §7.2 |

**结论 [源码契约]**：
- 本库把"传输顺序"完全让渡给上层，把"解码顺序"压缩为"AP 内部可表达的间隙"。
- 由于 `H265Depacketizer` 的公开签名是 `Unmarshal([]byte) ([]byte, error)`，**没有任何途径把 DON 交给调用方**。想要真正按 DON 重排，只能使用 `Deprecated` 的逐包类型（`H265SingleNALUnitPacket.DONL()`、`H265AggregationPacket.FirstUnit().DONL()` / `OtherUnits()[].DOND()`），且 `H265Packet.Packet()` 恒返回 `nil`（`:1725`，注释明示）。
- 因此本库的**可判定适用范围是 `sprop-max-don-diff = 0`**：此时"传输顺序 == 解码顺序"由 RFC §5 强制，丢弃 DON 不损失信息。

> RFC §5 原文：*"If sprop-max-don-diff is greater than 0 ... the transmission order of NAL units ... MAY be different than the NAL unit decoding order ... Otherwise ... the transmission order ... MUST be the same as the NAL unit decoding order."*

---

## 5. 协商启用 / 未启用 DONL 的包解释对比

`hasDonl` 是 `H265Depacketizer` 的**非导出**布尔字段（`:1550-1556`）。公开的设置入口只有 `H265Packet.WithDONL`（`:1711-1715`，`H265Packet` 已 `Deprecated`）。`H265Depacketizer` 自身**没有任何导出设置方法** → **[源码契约]**：直接使用 `H265Depacketizer` 的调用方在编译期就被锁定为 `hasDonl = false` 语义。

### 5.1 同一字节串的两种解释

| 封装 | `hasDonl = false` | `hasDonl = true` |
| --- | --- | --- |
| Single NAL | `hdr(2) + payload` | `hdr(2) + DONL(2) + payload` |
| AP | `hdr(2) + [len(2) AU]*` | `hdr(2) + DONL(2) + len(2) AU + [DOND(1) len(2) AU]*` |
| FU 首片 | `hdr(2) + fu(1) + payload` | `hdr(2) + fu(1) + DONL(2) + payload` |
| FU 续片 | `hdr(2) + fu(1) + payload` | `hdr(2) + fu(1) + payload`（启发式判定，`:1620`） |
| **PACI→FU 任意片** | 不读 DONL | **总是**读 DONL，忽略 S 位（`:1328-1334`） |
| PACI→Single / PACI→AP | 不读 DONL | 读 DONL（`:1306-1312`、`:1350-1356`） |

### 5.2 错配的实际后果（均为公开 API 复现）

**(a) 线上有 DONL、`hasDonl=false` ⇒ DONL 字节进入媒体**

```
输入: 26 01 | 12 34 | DE AD        （NAL 头 + DONL=0x1234 + 载荷）
输出: 00 00 00 01 | 26 01 | 12 34 DE AD
```
`parseH265SingleNalUnitPacket(..., false)` 把 `12 34` 当作载荷，`toAnnexB` 只剥离 `donl` 字段（为 nil），于是 2 字节噪声进入 Annex-B。**无任何错误返回**。

**(b) 线上无 DONL、`hasDonl=true` ⇒ 吃掉 2 字节载荷**

```
输入: 01 01 | AA BB CC
输出: 00 00 00 01 | 01 01 | CC        （AA BB 被当作 DONL 丢弃）
```
若载荷 < 2 字节则返回 `errShortPacket`（`:182-184`）。

**(c) AP 的两种解释在真机上通常互斥，但可构造共存**

- 随机搜索 20000 例：`hasDonl=false` 下合法的 AP，在 `hasDonl=true` 下 0 例静默成功、13508 例报错；反向亦然。
- 但**可构造**同一 15 字节在两种解释下都合法：

```
60 01              AP 头 (Type=48, layer=0, tid=1)
00 06              [无DONL] AU1 len=6   /  [有DONL] DONL=6
00 03              [无DONL] AU1 NAL 头  /  [有DONL] AU1' len=3
26 01 AA           [无DONL] AU1 载荷   /  [有DONL] AU1' = 26 01 AA
00                 [无DONL] AU1 载荷末 /  [有DONL] DOND=0
00 03 26 01 BB     [无DONL] AU2        /  [有DONL] AU2'
```

无 DONL 解释得到 2 个 NAL（Type=0 + Type=19）；有 DONL 解释得到 2 个 NAL（均为 Type=19）。**两者都无错返回，媒体内容不同。**

> **[需互操作证据]**：该歧义无法在带内消解。判别依据只能是 SDP 的 `sprop-max-don-diff`（RFC §7.1："When not present, the value ... is inferred to be equal to 0"）。本仓库不解析 SDP，因此**即使双方都"实现正确"，只要 `sprop-max-don-diff` 的取值在两端不一致，就会出现静默的媒体损坏**。

### 5.3 跨包状态与失效边界

| 状态 | 生命周期 | 置位点 | 失效/清空点 |
| --- | --- | --- | --- |
| `H265Depacketizer.partials []h265FragmentationPacket` | 单个分片 NAL 的组装窗口 | 续片追加 `:1603` | ① 单 NAL `:1559`；② AP `:1566`；③ PACI→Single `:1559`；④ PACI→AP `:1566`；⑤ 新 S 片 `:1597-1598`；⑥ E 成功 `:1591-1592` |
| `H265Depacketizer.hasDonl` | 实例生命周期 | 构造 / `H265Packet.WithDONL` | **永不改变**；无重协商检测 |
| `H265Payloader.cache *h265ParamSetCache` | 到下一个 VCL NAL 为止 | `cacheParameterSet` `:1789-1797` | VCL 到达 `:1899`；`SkipAggregation` 每次 `:1856` |
| `H265Payloader` 的 DON 状态 | —— | **不存在** | —— |

**边界行为（公开 API 复现）**

| 场景 | 结果 |
| --- | --- |
| E 片到达而 `partials` 为空（首片丢失） | 静默返回空，**不报错**（`:1581-1583`） |
| 非 S/非 E 片到达且缓冲为空 | `errExpectFragmentationStartUnit`（`:1599-1601`） |
| 新 S 片在缓冲非空时到达 | 静默丢弃旧序列（`:1597-1598`） |
| 单 NAL / AP 插在分片序列中间 | `partials` 被清空；随后 E 片静默丢弃 |
| 解析出错 | `partials` **不清空**（`:1621-1624` 直接返回）→ 后续 E 片可与旧片拼接 |
| 不同 NAL 的 E 片拼到旧 S 片 | 无 FuType / 序列号 / 时间戳校验，"成功"重建 |

最后一条的复现：
```
S 片 (type 19, 载荷 AA)  →  out = (空)
E 片 (type 19, 载荷 BB)  →  out = 00 00 00 01 26 01 AA BB
```
即使把 E 片换成 `layer=2/tid=0/FuType=1`，结果仍为 `26 09 AA BB`（保留 S 片的 `layer=1/tid=3`）。

> **[源码契约]**：以上均由 RFC 允许的"丢片后丢弃后续片"退化而来，但本实现是**静默**的；RFC §4.4.3 要求的是 *"the receiver SHOULD discard all following fragmentation units"*，本实现丢弃的是**前面的**缓冲，并把跨 NAL 的片拼接。

---

## 6. 失效点详表（DONL 相关）

### 6.1 FU 首片丢失 ⇒ 续片被当作首片解析（有 DONL 流）

```
输入序列： [S 丢失]  [续片 S=0,E=1, 载荷 BB CC DD]
hasDonl=true → parseDonl = (partials==0 && true) = true
⇒ 前 2 字节被当 DONL 吃掉，且 handleFragmentationUnit 因 E 且 partials==0 直接丢弃
```
结果：整个 NAL 静默丢失（若载荷 ≥2 字节），或 `errShortPacket`。

### 6.2 S 片在旧序列未完成时到达 ⇒ 其 DONL 泄漏进载荷

```
pkt0: 62 01 93 | 00 07 | AA     S=1, DONL=7   → partials=1, out 空
pkt1: 62 01 93 | 01 02 | BB     S=1, DONL=0x0102, 但 parseDonl=false（缓冲非空）
pkt2: 62 01 53 | CC             E=1
输出: 00 00 00 01 | 26 01 | 01 02 BB CC     ← 01 02 变成载荷
```
公开 API 复现（`H265Packet.WithDONL(true)`）。

### 6.3 PACI 包裹的 FU 续片 ⇒ 无条件吞掉 2 字节

`parseH265PACIPacket`（`:1319-1340`）在 `withDONL` 时对**任何** PACI→FU 都读 DONL，完全无视 `S`：

```go
case h265NaluFragmentationUnitType:
    ...
    if withDONL {
        donlVal := binary.BigEndian.Uint16(innerNalu[1:3])   // :1330-1332
        innerPayloadStart += h265NaluDonlSize                 // :1333
    }
```

而 RFC §4.4.3 规定 *"If ... the S bit is equal to 0 ... the DONL field MUST NOT be present in the FU."*

复现（`H265Packet.WithDONL(true)`，首片为普通 FU S=1）：
```
首片: 62 01 93 | 01 02 | AA        （DONL=0x0102）
续片: 64 01 | 62 00 | 53 | BB CC DD EE   （PACI cType=49, FU S=0,E=1）
输出: 00 00 00 01 | 26 01 | AA DD EE     ← BB CC 被当作 DONL 丢弃
```
若续片只带 2 字节载荷，则 `errShortPacket`。

> **[源码契约]**：这是本实现与 RFC 的**直接冲突**（不是启发式的边界情况）。一个符合 RFC 的对端在 `sprop-max-don-diff>0` 的会话里必然发送"无 DONL 的 PACI 续片"，本实现必然错位。→ 属 **[需互操作证据]** 才能确定实际影响，但冲突本身在源码层可判定。

### 6.4 PACI 内层 FU 首片（S=1）反而**需要** DONL，能正常工作

同一复现的对照：`PACI(FU S=1)` + DONL 时，解析得到正确 DONL（`fuHeader=d3`，`payload=aabb`）。这与 6.3 的"无条件"是同一段代码的两种表现。

---

## 7. 聚合层标识缺失/不一致时可保持的媒体语义

### 7.1 可保持（[源码契约] + 公开复现）

| 情形 | 媒体结果 | 依据 |
| --- | --- | --- |
| AP PayloadHdr 的 LayerID/TID 与 AU 不一致 | **每个 AU 自己的 LayerID/TID 精确保留** | AU 自带 2 字节 NAL 头，`split` 逐 AU 解析 `:569` |
| AP PayloadHdr 的 F=1 而 AU F=0 | AP 层接受（不校验 F）；媒体与 F=0 时相同 | `parseH265AggregationPacket` 不查 F `:379-383` |
| AP PayloadHdr 缺失语义（例如被中间盒改写） | 不影响媒体 NAL 头的 LayerID/TID | 同上 |
| AP 内 AU 数 ≥ 2，且 AU 的 Type ≠ 48/49/50 | 正常展开 | `parseH265SingleNalUnitPacket` 拒绝 48/49/50 `:192-194` |
| AP 内出现 Type=48 或 49 | 报 `errInvalidH265PacketType` | 同上 |
| 分片链的 LayerID/TID 分歧 | 取**首片**，其余静默忽略 | `:1015` |
| PACI 包裹：内层 LayerID/TID | 由 PACI PayloadHdr 提供，正确保留 | `rebuildPACIHeader` `:1246-1261` |

### 7.2 不可保持

| 情形 | 损失 | 证据等级 |
| --- | --- | --- |
| 无 DONL 且传输顺序 ≠ 解码顺序 | 解码顺序不可恢复（无带内信息） | [源码契约] |
| `H265Depacketizer` 路径 | DON 计算后被丢弃，调用方不可见 | [源码契约] |
| PACI 内层 NAL 的 **F 位** | 丢失：`A()` 实际读取的是 Y 位，`rebuildPACIHeader` 恒置 F=0 | [源码契约]（见 §10.2） |
| 跨 NAL 的 DON 连续性 | 不检查；相邻 NAL 的 DON 无任何交叉验证 | [源码契约] |
| AP 单位的解码顺序编号 | RFC 中 AP 本身无 DON，本实现一致（不是损失） | [源码契约] |
| 接收方按 AP PayloadHdr TID 做层过滤 | 只能看到 min（最保守），无法分辨具体 AU | [跨层推断] |

### 7.3 关于"聚合层标识"的定位

AP PayloadHdr 的 LayerID/TID 在本库中是**纯发送侧/纯 RTP 层元数据**：

- 发送侧：用于在 RTP 层表达"这个 AP 最不重要的 TID 是多少"（min）。
- 接收侧：从不读取，也从不校验。

因此：

- 对**媒体语义**：缺失/不一致**无损**（可保持）。
- 对**RTP 层策略**（按 TID 丢弃、按 LayerID 转发）：该值若被篡改，本库**无法察觉**，也不会报错。这是 **[跨层推断]** 的风险面：SFU 若信任该字段做拥塞丢弃，错误值会导致错误裁剪，但不会导致解析失败。

---

## 8. 可判定的兼容条件

以下每条都是可机械检查的谓词（对给定字节流或给定配置可直接判定）。

| # | 条件 | 判定方法 | 证据等级 |
| --- | --- | --- | --- |
| **C1** | `H265Depacketizer.hasDonl == (sprop-max-don-diff > 0)` | 检查 SDP 参数与接收器配置是否一致；本库无自动推导 | [需互操作证据] |
| **C2** | 流中所有 Single/AP/FU/PACI 的 DONL 存在性与 C1 一致 | 逐包检查 DONL 是否可读、解析是否 `errShortPacket`；注意 §5.2(c) 的构造歧义使"两边都不报错"不能证明一致 | [需互操作证据] |
| **C3** | 若 `sprop-max-don-diff > 0`，则本库的 `H265Payloader` 输出**不合规** | `AddDONL` 无效果（`:1744`）；构造的 AU 恒 `donl=nil`（`:1849-1853`） | [源码契约] |
| **C4** | AP PayloadHdr.LayerID/TID == 全 AU 的最小值 | 解码后比较；本库发送侧满足，接收侧不校验 | [源码契约] |
| **C5** | AP 内 AU 顺序 == 解码顺序 | 由 §2.1 的 `dond ≥ 0` 强制；任何逆序 DON 在发送侧报 `errDonlOutOfOrder` | [源码契约] |
| **C6** | AP 内 DON 单调递增且差 ∈ [1,256] | 同上；接收侧 `uint16` 加法可表达回绕，发送侧不能 | [源码契约] |
| **C7** | FU 的 DONL 仅出现在 S=1 的片 | 发送侧满足（`:965-992`）；接收侧对普通 FU 用启发式近似（`:1620`），对 PACI→FU 违反（§6.3） | [源码契约] |
| **C8** | 若使用 PACI 包裹 FU 且 `sprop-max-don-diff>0`，则续片必须带 DONL 才能被本库正确解析 | 用一条"续片无 DONL"的测试向量即可判定（会丢 2 字节或报错） | [源码契约] |
| **C9** | 本库可解码的 AP 中不得含 F=1 的 AU；但本库的 Payloader 可产生此类 AP | `H265Payloader.Payload` 输入含 F=1 NAL → AP 头 F=1 → `split` 内 `parseH265SingleNalUnitPacket` 报 `errH265CorruptedPacket` | [源码契约] |
| **C10** | 每次 `H265Payloader.Payload` 调用只应包含一个 AU | 发送侧不检测 AU 边界（唯一近似是第一片的 `first_slice_segment_in_pic_flag`，且仅用于参数集 ID 解析）；一个调用内两个 VCL NAL 会被聚合进同一 AP | [跨层推断] + 公开复现（见 §9.2） |
| **C11** | `IsPartitionHead/IsPartitionTail` 对 PACI 流不可信 | 见 §10.1，直接用公开 API 判定 | [源码契约] |
| **C12** | 需要跨包 DON 重排的调用方必须使用 `Deprecated` 的逐包类型；`H265Depacketizer` 不可满足 | 检查 API 面：`H265Depacketizer` 无 `DONL()` 导出 | [源码契约] |

---

## 9. 其它可见的不一致（与顺序信息交互）

### 9.1 F 位：发送可产出、接收必然拒绝 [源码契约 / 公开复现]

```go
// 发送：任一 AU F=1 则 AP F=1
if packet.payloadHeader.F() { fBit = true }        // :500-502
if fBit { header |= uint16(0b1) << 15 }            // :521-523
```

```go
// 接收：AP 层不查 F；但每个 AU 经 parseH265SingleNalUnitPacket 时
if payloadHeader.F() { return nil, errH265CorruptedPacket }   // :188-190
```

公开复现：
```go
nal1 := []byte{00,00,00,01, 26,01,AA}
nal2 := []byte{00,00,00,01, A6,01,BB}   // F=1
out := (&codecs.H265Payloader{}).Payload(1500, append(nal1, nal2...))
// out[0] = e0 01 00 03 26 01 AA 00 03 a6 01 BB   （AP F=1）
(&codecs.H265Depacketizer{}).Unmarshal(out[0])   // errH265CorruptedPacket
```

补充：`Deprecated` 的 `H265AggregationPacket.Unmarshal` 会**在 AP 层**拒绝 F=1（`:715-717`），而私有解析器不会——两个解析器行为不同。

### 9.2 参数集缓存会改变 NAL 的相对位置 [源码契约]

`H265Payloader` 把 VPS/SPS/PPS 缓存起来，直到下一个 VCL NAL 才按缓存顺序前置（`:1895-1900`）。本地复现：输入 `SPS, PPS, SPS', SEI, VCL` → 输出 `PPS, SPS', SEI, VCL`。因为这些都在同一 AU 内且均为非 VCL，[跨层推断] 认为不改变 VCL 的解码顺序；但它确实改变了**传输顺序**，因此若调用方在同一 `Payload()` 中混入多个 AU，会把不同 AU 的 NAL 交错。

未跟随的缓存**永不冲刷**：只送入 SPS 时输出 0 个 payload（复现）。这意味着参数集可能被无限期扣留。

### 9.3 PACI 头字段访问器全部读错位 [源码契约 / 公开复现]

```go
func (h *paciHeaderFields) A()  bool { return (uint16(*h) & 0b1 << 15) != 0 }  // :1152-1154
func (h *paciHeaderFields) F0() bool { return (uint16(*h) & 0b1 << 3)  != 0 }  // :1168-1170
func (h *paciHeaderFields) F1() bool { return (uint16(*h) & 0b1 << 2)  != 0 }  // :1172-1174
func (h *paciHeaderFields) F2() bool { return (uint16(*h) & 0b1 << 1)  != 0 }  // :1176-1178
func (h *paciHeaderFields) Y()  bool { return (uint16(*h) & 0b1)       != 0 }  // :1180-1182
```

Go 中 `&` 与 `<<` 同优先级、左结合，故 `h & 0b1 << 15` ≡ `(h & 1) << 15`。**五个访问器都在测试 bit 0（即 Y 位）**。`CType()` / `PHSize()` 因先算 `mask` 再运算而正确。

复现：

| 仅置位 | `A()` | `F0()` | `F1()` | `F2()` | `Y()` |
| --- | --- | --- | --- | --- | --- |
| 0x8000 (A) | false | false | false | false | false |
| 0x0008 (F0) | false | false | false | false | false |
| 0x0004 (F1) | false | false | false | false | false |
| 0x0002 (F2) | false | false | false | false | false |
| 0x0001 (Y) | **true** | **true** | **true** | **true** | true |

后果：

1. `rebuildPACIHeader` 用 `A()` 还原内层 F（`:1247-1250`）⇒ **PACI 包裹的 NAL 的 F 恒为 0**。复现：A=1、cType=19 的 PACI → 重建 NAL 头 `26 1c`（F=0）。
2. `TSCI()` 以 `F0()` 为前提（`:1237`）⇒ **合规 PACI（F0=1, Y=0）永远返回 nil**；只有非法的 Y=1 才返回 TSCI。
3. `TSCI()` 的组装与访问器错位 8 bit：`phes[0]<<16 | phes[1]<<8 | phes[2]`（`:1241`）是 24 bit 布局，而 `TL0PICIDX()`（`:1490`）读 bit 31:24、`IrapPicID()`（`:1498`）读 bit 23:16。复现（PHES=`CA FE 80`）：`TL0PICIDX=0x00`（应为 0xCA）、`IrapPicID=0xCA`（应为 0xFE）。单元测试 `TestH265_TemporalScalabilityControlInformation` 使用 32 bit 布局（`0xCA<<24 | 0xFE<<16`），因此测试通过而实际解析错位。

> 与本题的关系：TSCI 的 `S`/`E` 是"某 NAL 是否为图像首/末 VCL"的带内信号，是 RFC 7798 中除 DON 之外唯一与"图像级顺序/边界"相关的字段。它目前不可用。

### 9.4 分片边界判定不进入 PACI [源码契约 / 公开复现]

```go
func (d *H265Depacketizer) IsPartitionHead(payload []byte) bool {
    header := H265NALUHeader(...)
    if header.IsFragmentationUnit() { ... return fuHeader.S() }
    return true                                   // :1672-1687
}
```

PACI（Type=50）不在 `IsFragmentationUnit()` 分支内，因此：

| 载荷 | `IsPartitionHead` | `IsPartitionTail(false, …)` |
| --- | --- | --- |
| 普通 FU S=0,E=1 | false | **true** |
| PACI(FU S=0,E=1) | **true**（错误） | **false**（错误） |
| PACI(FU S=1,E=0) | true | false |

这直接影响调用方对"分区（≈AU/NAL）边界"的判定，即**把传输顺序映射为视频帧边界**的那一步。属 [源码契约]；对上游 `SampleBuilder` 的实际影响属 [跨层推断]。

---

## 10. 总体判断

1. **LayerID/TID 的保留是可靠的**，因为它是随包自带的 16 位头，且本库在多处采用"整个 uint16 原样搬运"的策略（单 NAL 复用字段、FU 用掩码只改 Type、AP 逐 AU 解析）。唯一在保留链上出问题的是 **PACI 的 F 位**（`A()` 读错位），而 F 不是 LayerID/TID。
2. **解码顺序信息的保留是名义上的、不是可用的**：DON 能被解析、能在 AP 内重建，但在媒体写出前被显式剥离；跨包没有任何 DON 状态。因此本库**只能服务 `sprop-max-don-diff = 0` 的会话**。
3. **发送与接收在 DONL 上的能力不对称**：发送侧根本不产生 DONL/DOND（`AddDONL` 无效），接收侧则对 DONL 位置做了两处与 RFC 冲突/近似的假设（普通 FU 用缓冲空否启发式、PACI→FU 无条件读）。这决定了"本库自环"只在无 DONL 时闭合。
4. **需要互操作证据才能定论的部分**集中在：SDP 参数与 `hasDonl` 是否一致、对端是否发送 PACI→FU 续片、是否存在 §5.2(c) 的字节歧义碰撞、以及上游是否真的按 TID 做层过滤。其余均可由源码或本地复现判定。

---

## 附录：复现方式

所有复现均在包外用 `codecs.H265Payloader` / `codecs.H265Depacketizer` / `codecs.H265Packet`（`WithDONL`）/ `codecs.H265PACIPacket` 构造，未修改仓库文件。运行环境：

```
GOROOT=/Users/dengquan/.local/sdk/go1.25.9
GOPATH=/Users/dengquan/.claude-fb2-tech-8285/go
GOPROXY=off  GOFLAGS=-mod=mod
go test ./codecs/ -run TestH265
```

原有用例（`TestH265_AggregationRoundtrip`、`TestH265Depacketizer_Roundtrip` 等）全部通过；上述结论针对的是既有用例未覆盖的组合（DONL 与 PACI、DONL 与丢片、AP 头与 AU 头不一致、有 DONL 的 AP 回绕等）。
