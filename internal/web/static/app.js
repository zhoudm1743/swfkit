"use strict";

/* ============================================================
   swfkit Web UI —— 单文件 Vue（全局构建，无 webpack/vite）
   页面骨架见 index.html（仅一个挂载点），全部界面与逻辑在本文件。
   ============================================================ */

/* ---- WASM 内存捕获（必须在加载 Ruffle 之前安装）----
   挂钩 WebAssembly 实例化与 Memory 构造，收集页面上出现过的所有线性内存，
   运行时模式的扫描器直接在这些内存里搜值/写值。 */
  (function installWasmHooks() {
    if (window.__wasmMemories) return;
    const memories = new Set();
    window.__wasmMemories = memories;
    const OrigMemory = WebAssembly.Memory;
    const HookedMemory = function (...args) {
      const m = new OrigMemory(...args);
      memories.add(m);
      return m;
    };
    HookedMemory.prototype = OrigMemory.prototype;
    Object.setPrototypeOf(HookedMemory, OrigMemory);
    WebAssembly.Memory = HookedMemory;

    function collect(result) {
      try {
        const inst = result && result.instance;
        if (inst && inst.exports) {
          for (const k in inst.exports) {
            const v = inst.exports[k];
            if (v instanceof OrigMemory) memories.add(v);
          }
        }
      } catch (e) { /* 忽略收集失败 */ }
      return result;
    }
    const origInst = WebAssembly.instantiate;
    WebAssembly.instantiate = function (...args) {
      const r = origInst.apply(WebAssembly, args);
      if (r && typeof r.then === "function") return r.then(collect);
      return collect(r);
    };
    if (WebAssembly.instantiateStreaming) {
      const origS = WebAssembly.instantiateStreaming;
      WebAssembly.instantiateStreaming = function (...args) {
        return origS.apply(WebAssembly, args).then(collect);
      };
    }
  })();

/* ---- Ruffle 惰性加载（CDN；离线时试玩不可用）---- */
let ruffleLoading = null;

/* ---- Ruffle 控制台环形缓冲：检测资源加载失败并给出指引 ---- */
window.__rtLog = [];
(() => {
  for (const m of ["log", "info", "warn", "error"]) {
    const orig = console[m].bind(console);
    console[m] = (...a) => {
      try {
        window.__rtLog.push(a.map(x => String(x)).join(" "));
        if (window.__rtLog.length > 60) window.__rtLog.shift();
      } catch (e) { /* 忽略 */ }
      orig(...a);
    };
  }
})();

function loadRuffle() {
  if (window.RufflePlayer) return Promise.resolve();
  if (!ruffleLoading) {
    ruffleLoading = new Promise((resolve, reject) => {
      const s = document.createElement("script");
      s.src = "https://cdn.jsdelivr.net/npm/@ruffle-rs/ruffle";
      s.onload = () => setTimeout(resolve, 80); // 等待内部注册
      s.onerror = () => { ruffleLoading = null; reject(new Error("Ruffle CDN 加载失败（当前离线？）")); };
      document.head.appendChild(s);
    });
  }
  return ruffleLoading;
}

/* ---- 修改表：以游戏内容哈希为钥匙的持久化清单 ---- */
function fnvKey(u8) {
  let h = 0x811c9dc5;
  const step = Math.max(1, Math.floor(u8.length / 4096));
  for (let i = 0; i < u8.length; i += step) { h ^= u8[i]; h = Math.imul(h, 0x01000193); }
  if (u8.length > 64) { h ^= u8[u8.length - 64]; h = Math.imul(h, 0x01000193); }
  return "g" + u8.length.toString(36) + "-" + (h >>> 0).toString(36);
}

/* ---- 指针链：跟随 WASM 堆内 32 位指针偏移读写 ---- */
function chainFollow(m, chain) {
  const u32 = new Uint32Array(m.buffer);
  let a = chain.slots[0];
  for (let i = 0; i < chain.offs.length; i++) {
    if (a < 0 || a + 4 > m.buffer.byteLength) return null;
    a = u32[a >>> 2] + chain.offs[i];
  }
  if (a < 0 || a + 8 > m.buffer.byteLength) return null;
  return a; // 最终地址
}
function chainRead(m, chain) {
  const a = chainFollow(m, chain);
  if (a === null) return null;
  try {
    const dv = new DataView(m.buffer);
    return chain.type === "i32" ? dv.getInt32(a, true) : dv.getFloat64(a, true);
  } catch (e) { return null; }
}
function chainWrite(m, chain, v) {
  const a = chainFollow(m, chain);
  if (a === null) return false;
  try {
    const dv = new DataView(m.buffer);
    if (chain.type === "i32") dv.setInt32(a, v | 0, true);
    else dv.setFloat64(a, v, true);
    return true;
  } catch (e) { return false; }
}

/* ---- 指针扫描：反向查找“谁指向这个地址”，构建稳定指针链 ---- */
const sleep0 = () => new Promise(r => setTimeout(r, 0));

async function rtPointerScan(m, targetAddr, opts = {}) {
  const report = opts.onProgress || (() => {});
  const maxOff = opts.maxOff ?? 0x1000;   // 结构字段偏移上限
  const maxDepth = opts.maxDepth ?? 2;    // 链深度（2 = 根槽 → 结构槽 → 目标）
  const size = m.buffer.byteLength;
  const u32 = new Uint32Array(m.buffer);
  report("指针扫描：第 1 层…");
  await new Promise(r => setTimeout(r, 0));

  // 层 1：值域 [T - maxOff, T] 的槽位（指向含目标字段的结构）
  let slots = [];      // { slot, f }  f = T - u32[slot]
  const tMin = Math.max(0x100, targetAddr - maxOff);
  for (let i = 0; i + 4 <= u32.length * 4; i += 4) {
    const v = u32[i >>> 2];
    if (v >= tMin && v <= targetAddr) {
      slots.push({ slot: i, f: targetAddr - v });
      if (slots.length >= 50000) break;
    }
    if ((i & 0x3FFFFF) === 0) await sleep0();
  }
  if (!slots.length) return { depth1: [], depth2: [] };
  report(`指针扫描：第 2 层（${slots.length} 个一级槽位）…`);
  await new Promise(r => setTimeout(r, 0));

  // 层 2：值域覆盖某个层 1 槽位地址的槽位 → 构成二级链
  const sorted = slots.map(s => s.slot).sort((a, b) => a - b);
  const depth2 = [];
  const chains = { depth1: slots.map(s => ({ slots: [s.slot], offs: [s.f] })), depth2 };
  for (let i = 0; i + 4 <= u32.length * 4; i += 4) {
    const v = u32[i >>> 2];
    if (v < 0x100 || v % 8 !== 0) continue;
    // 二分找覆盖 v 的层 1 槽位地址 A（A ∈ [v, v + maxOff]）
    let lo = 0, hi = sorted.length - 1, hit = -1;
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      if (sorted[mid] < v) lo = mid + 1; else { hit = sorted[mid] >= v ? sorted[mid] : hit; hi = mid - 1; }
    }
    if (hit < 0 || hit > v + maxOff) continue;
    const s1 = hit;
    depth2.push({ slots: [i, s1], offs: [s1 - v, slots.find(s => s.slot === s1).f] });
    if (depth2.length >= 2000) break;
    if ((i & 0x3FFFFF) === 0) { report(`指针扫描：第 2 层 ${depth2.length} 链…`); await sleep0(); }
  }
  return chains;
}

/* ---- 内存注册表与清单读写（Memory 对象不进响应式状态）---- */
const memIds = new WeakMap();
let memSeq = 0;
function memIdOf(m) {
  if (!memIds.has(m)) memIds.set(m, ++memSeq);
  return memIds.get(m);
}
function memById(id) {
  for (const m of rtMemories()) {
    if (memIdOf(m) === id) return m;
  }
  return null;
}
function cheatRead(c) {
  const m = memById(c.memId);
  if (!m) return null;
  if (c.type === "ptr") return chainRead(m, c.chain);
  try {
    const dv = new DataView(m.buffer);
    return c.type === "i32" ? dv.getInt32(c.addr, true) : dv.getFloat64(c.addr, true);
  } catch (e) { return null; }
}
function cheatWrite(c, v) {
  const m = memById(c.memId);
  if (!m) return false;
  if (c.type === "ptr") return chainWrite(m, c.chain, v);
  try {
    const dv = new DataView(m.buffer);
    if (c.type === "i32") dv.setInt32(c.addr, v | 0, true);
    else dv.setFloat64(c.addr, v, true);
    return true;
  } catch (e) { return false; }
}

function fmtSize(n) {
  if (n >= 1048576) return (n / 1048576).toFixed(1) + " MB";
  if (n >= 1024) return (n / 1024).toFixed(0) + " KB";
  return n + " B";
}

/* ---- API 封装 ---- */
async function api(method, url, body, raw) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    if (raw) {
      opts.body = body;
      if (body instanceof File || body instanceof Blob) {
        opts.headers["X-Filename"] = body.name || "";
      }
    } else {
      opts.body = JSON.stringify(body);
      opts.headers["Content-Type"] = "application/json";
    }
  }
  const resp = await fetch(url, opts);
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

/* ============================================================
   SOL 树节点（递归组件）
   ============================================================ */
const TreeNode = {
  name: "tree-node",
  props: { name: String, value: null, path: String, filter: String },
  emits: ["edit"],
  data() {
    return { open: true, editing: false, draft: "" };
  },
  computed: {
    isAnnotated() {
      const v = this.value;
      return v !== null && typeof v === "object" && !Array.isArray(v) &&
        ("$undefined" in v || "$date" in v || "$bytes" in v || "$xml" in v);
    },
    isContainer() {
      const v = this.value;
      if (this.isAnnotated) return false;
      return v !== null && typeof v === "object";
    },
    entries() {
      const v = this.value;
      const base = this.path ? this.path + "." : "";
      if (Array.isArray(v)) {
        return v.map((item, i) => ({ key: String(i), val: item, path: base + i }));
      }
      const out = [];
      for (const k of Object.keys(v)) {
        if (k === "$ecma" || k === "$class") continue;
        out.push({ key: k, val: v[k], path: base + k });
      }
      return out;
    },
    typeDesc() {
      const v = this.value;
      if (!this.isContainer) return "";
      const keys = Object.keys(v).filter(k => k !== "$ecma" && k !== "$class");
      if (v.$class) return `class ${v.$class}{${keys.length}}`;
      if (v.$ecma) return `ECMA[${keys.length}]`;
      return Array.isArray(v) ? `Array(${v.length})` : `{${keys.length}}`;
    },
    typeName() {
      const v = this.value;
      if (v === null) return "";
      if (this.isAnnotated) {
        if ("$undefined" in v) return "undefined";
        if ("$date" in v) return "Date";
        if ("$bytes" in v) return `ByteArray(${Math.floor(atob(v.$bytes).length)}B)`;
        if ("$xml" in v) return "XML";
      }
      return typeof v;
    },
    valText() {
      const v = this.value;
      if (v === null) return "null";
      if (this.isAnnotated) {
        if ("$undefined" in v) return "undefined";
        if ("$date" in v) return new Date(v.$date).toISOString();
        if ("$bytes" in v) return `<${atob(v.$bytes).length} 字节>`;
        if ("$xml" in v) return v.$xml;
      }
      if (typeof v === "string") return JSON.stringify(v);
      return String(v);
    },
    valCls() {
      const v = this.value;
      if (typeof v === "boolean") return "bool";
      if (typeof v === "string") return "str";
      if (v === null || this.isAnnotated) return "raw";
      return "";
    },
    rowText() {
      return (this.name + " " + this.valText + " " + this.typeName).toLowerCase();
    },
  },
  methods: {
    startEdit() {
      const v = this.value;
      this.draft = typeof v === "string" ? v : (v === null ? "null" : String(v));
      this.editing = true;
    },
    commit() {
      this.editing = false;
      this.$emit("edit", { path: this.path, value: this.draft });
    },
  },
  template: `
    <div class="node">
      <template v-if="isContainer">
        <div class="node-row">
          <span class="caret" @click="open = !open">{{ open ? '▾' : '▸' }}</span>
          <span class="key">{{ name }}</span>
          <span class="type">{{ typeDesc }}</span>
        </div>
        <div class="children" v-show="open">
          <tree-node v-for="e in entries" :key="e.path" :name="e.key" :value="e.val"
                     :path="e.path" :filter="filter" @edit="$emit('edit', $event)" />
        </div>
      </template>
      <template v-else>
        <div class="node-row" v-show="!filter || rowText.includes(filter.toLowerCase())">
          <span class="caret"></span>
          <span class="key">{{ name }}</span>
          <span class="type">{{ typeName }}</span>
          <span class="val" :class="valCls" @click="startEdit">{{ valText }}</span>
          <input v-if="editing" class="edit-box" v-model="draft" v-focus
                 @keydown.enter="commit" @keydown.esc="editing = false" @blur="editing = false">
        </div>
      </template>
    </div>`,
};

/* ============================================================
   根应用
   ============================================================ */
const { createApp, nextTick } = Vue;

const TEMPLATE = `
<header>
  <h1>swfkit <span class="sub">Flash 游戏修改器</span></h1>
  <nav>
    <button :class="['tab', tab === 'rt' && 'active']" @click="tab = 'rt'">运行时修改</button>
    <button :class="['tab', tab === 'swf' && 'active']" @click="tab = 'swf'">静态补丁</button>
    <button :class="['tab', tab === 'sol' && 'active']" @click="tab = 'sol'">SOL 存档</button>
  </nav>
</header>

<main>
  <!-- ============ 运行时修改（内嵌 Ruffle + 内存扫描） ============ -->
  <section v-show="tab === 'rt'" @dragover.prevent @drop.prevent="rtDrop">
    <div class="upload-row">
      <label class="file-btn">📂 {{ rtFile ? rtFile.name : '选择 SWF 文件' }}<input type="file" accept=".swf" @change="onRtFilePicked"></label>
      <input v-model="rtPath" class="path-input" placeholder="或粘贴本地 .swf 路径（支持游戏目录内的相对资源）" spellcheck="false">
      <button class="primary" @click="loadMain">载入</button>
      <button @click="loadDemo('rt')">demo</button>
      <span class="status" :class="{ err: rtErr }">{{ rtStatus }}</span>
      <span class="spacer"></span>
      <button v-show="rtLoaded" @click="rtFullscreen">{{ stageFs ? '⤢ 退出全屏' : '⛶ 全屏' }}</button>
      <button v-show="rtLoaded" @click="rtStop">结束</button>
    </div>

    <div class="rt-stage" ref="stage">
      <div class="player-wrap">
        <div id="rt-box" ref="rtBox"></div>
        <div class="load-overlay" v-show="rtLoading">
          <div class="spinner"></div>
          <div class="stage">{{ rtLoadStage }}</div>
          <div class="bar"><div class="bar-in" :class="{ indet: rtLoadPct === null }" :style="{ width: (rtLoadPct ?? 100) + '%' }"></div></div>
          <div class="bytes" v-if="rtLoadBytes">{{ rtLoadBytes }}</div>
        </div>
      </div>

      <!-- 悬浮训练器：窗口模式在游戏下方；全屏时悬浮于游戏上方 -->
      <div class="trainer" v-show="rtLoaded" ref="trainerEl"
           :style="trainerStyle"
           @dblclick.self="trainerOpen = !trainerOpen">
        <div class="trainer-head" @mousedown.prevent="panelDragStart"
             @dblclick.self="trainerOpen = !trainerOpen">
          <span class="badge">训练器</span>
          <span class="tcount">{{ rtCountText }}</span>
          <span class="spacer"></span>
          <button @click="rtFullscreen">{{ stageFs ? '⤢ 退出全屏' : '⛶ 全屏' }}</button>
          <button @click="trainerOpen = !trainerOpen">{{ trainerOpen ? '▴ 收起' : '▾ 面板' }}</button>
        </div>
        <div class="trainer-body" v-show="trainerOpen">
          <div class="scan-info" v-show="rtSearching">{{ rtScanInfo }}</div>
          <div class="ptabs">
            <button :class="['ptab', ptTab === 'search' && 'on']" @click="ptTab = 'search'">搜索</button>
            <button :class="['ptab', ptTab === 'cheats' && 'on']" @click="ptTab = 'cheats'">
              清单<span v-if="cheats.length"> ·{{ cheats.length }}</span>
            </button>
          </div>

          <!-- 搜索页：CE 式单按钮智能搜索 -->
          <div class="searchpage" v-show="ptTab === 'search'">
            <div class="frow">
              <input class="fin grow" v-model="rtSearchValue" placeholder="数值（留空 = 未知初值扫描）">
              <select class="fsel" v-model="rtSearchType" style="width:74px; flex:none;" title="数值类型">
                <option value="auto">自动</option>
                <option value="f64">f64</option>
                <option value="i32">i32</option>
              </select>
            </div>
            <div class="frow">
              <button class="pbtn primary grow" :class="{ searching: rtSearching }" :disabled="rtSearching" @click="rtSmartSearch">{{ searchBtnText }}</button>
              <select class="fsel" v-model="rtNarrowMode" style="width:92px; flex:none;"
                      :disabled="!rtHasHits" title="再次搜索的缩小方式">
                <option value="exact">精确值</option>
                <option value="inc">变大了</option>
                <option value="dec">变小了</option>
                <option value="changed">变化了</option>
                <option value="same">没变化</option>
              </select>
              <button class="pbtn" style="flex:none" @click="rtResetSearch" title="清空结果，重新开始">重置</button>
            </div>
            <div class="hits-scroll">
              <table class="hits ttable">
                <thead><tr><th class="c-ty">类型</th><th class="c-addr">地址</th><th class="c-val">当前值</th><th class="c-add"></th></tr></thead>
                <tbody>
                  <tr v-for="(h, i) in rtDisplayHits" :key="h.key" style="cursor:pointer"
                      title="双击加入修改清单" @dblclick="addCheatFromHit(h)">
                    <td class="ty"><span class="tybadge">{{ h.type }}</span></td>
                    <td class="addr">0x{{ h.addr.toString(16) }}</td>
                    <td class="val" :class="{ err: h.cur === null }">{{ h.cur === null ? '⟲' : h.cur }}</td>
                    <td class="c-add">
                      <button class="mini" title="加入修改清单" @click.stop="addCheatFromHit(h)">＋</button>
                      <button class="mini ptr" title="追踪指针链（地址漂移时用这个）" @click.stop="ptrTrack(h)">🎯</button>
                    </td>
                  </tr>
                </tbody>
              </table>
              <div v-if="!rtHasHits" class="empty">
                输入数值点「搜索」；游戏里数值变化后再点一次即缩小范围。<br>
                血条/蓝条等看不见精确值：留空数值直接搜索，再配合上方下拉缩小
              </div>
            </div>
          </div>

          <!-- 修改清单页 -->
          <div class="panel-scroll" v-show="ptTab === 'cheats'">
            <table class="ctable">
              <thead><tr><th class="k">🔒</th><th>描述</th><th class="v">值</th><th class="op"></th></tr></thead>
              <tbody>
                <template v-for="c in cheats" :key="c.id">
                  <tr :class="{ fresh: freshId === c.id, dead: c.cur === null }">
                    <td class="k"><button :class="['lock', { on: c.lock }]" @click="toggleLock(c)"
                        :title="c.lock ? '解锁（停止冻结）' : '锁定（冻结当前值）'">{{ c.lock ? '🔒' : '🔓' }}</button></td>
                    <td><input class="cdesc" v-model="c.desc" placeholder="备注"></td>
                    <td class="v"><input class="cval-in" :class="{ lockv: c.lock }" :value="c.cur"
                        @focus="valFocusId = c.id" @blur="writeCheatVal(c, $event)"
                        @keydown.enter="$event.target.blur()"></td>
                    <td class="op">
                      <button class="mini" :class="{ on: cfgId === c.id }" @click="cfgId = cfgId === c.id ? '' : c.id"
                              title="步长与热键">⚙</button>
                      <button class="del" @click="delCheat(c.id)">✕</button>
                    </td>
                  </tr>
                  <tr v-if="cfgId === c.id" class="cfg">
                    <td class="k"></td>
                    <td class="cfg-cells" colspan="2">
                      <label class="hk">步长 <input class="hkin" style="width:56px" v-model.number="c.delta"></label>
                      <button class="mini" @click="adjustCheat(c, c.delta)">＋{{ c.delta }}</button>
                      <button class="mini" @click="adjustCheat(c, -c.delta)">－{{ c.delta }}</button>
                    </td>
                    <td class="op cfg-hks">
                      <input class="hkin" style="width:54px" :value="c.hotkey" @keydown.prevent="setHk(c, 'hotkey', $event)" placeholder="锁键" title="锁定开关热键">
                      <input class="hkin" style="width:54px" :value="c.hotkeyAdd" @keydown.prevent="setHk(c, 'hotkeyAdd', $event)" placeholder="＋键">
                      <input class="hkin" style="width:54px" :value="c.hotkeyDec" @keydown.prevent="setHk(c, 'hotkeyDec', $event)" placeholder="－键">
                    </td>
                  </tr>
                </template>
              </tbody>
            </table>
            <div v-if="!cheats.length" class="empty">
              清单为空：在「搜索」页结果行点「＋」加入地址，<br>然后可 🔒 锁定冻结、⚙ 配置步长与热键
            </div>
          </div>
        </div>
      </div>
    </div>
  </section>

  <!-- ============ 静态补丁工作台 ============ -->
  <section v-show="tab === 'swf'">
    <div class="upload-row">
      <label class="file-btn">📂 {{ swfFile ? swfFile.name : '选择 SWF 文件' }}<input type="file" accept=".swf" @change="e => swfFile = e.target.files[0]"></label>
      <button class="primary" @click="uploadSwf">上传解析</button>
      <button @click="loadDemo('swf')">试用内置 demo</button>
      <span class="status" :class="{ err: swfErr }">{{ swfStatus }}</span>
    </div>

    <div class="card" v-show="swfId">
      <div class="info-line">{{ swfSummary }}</div>
      <div v-for="(m, i) in swfModules" :key="i" class="mod-line">{{ modText(m) }}</div>
    </div>

    <div class="scan-row card" v-show="swfId">
      <label>当前值 <input v-model="swfScanValue" placeholder="如 100 / 3.14 / 0xFF"></label>
      <label>口径
        <select v-model="swfScanMode">
          <option value="any">全部</option>
          <option value="int">整型</option>
          <option value="uint">无符号</option>
          <option value="double">浮点</option>
        </select>
      </label>
      <button class="primary" @click="doScan(false)">扫描</button>
      <button @click="doScan(true)">二次缩小</button>
      <span class="spacer"></span>
      <label>新值 <input v-model="swfNewValue" placeholder="99999"></label>
      <button class="primary" @click="applyChecked">应用所选</button>
      <a v-if="swfDownloadUrl" class="button" :href="swfDownloadUrl">下载补丁后 SWF</a>
      <button @click="playStatic">▶ 试玩当前版本</button>
    </div>

    <div class="card" v-show="swfHits.length">
      <div class="hits-head"><span class="status">{{ hitSummary }}</span></div>
      <div class="hits-scroll" style="max-height:420px">
      <table class="hits">
        <thead><tr><th></th><th>#</th><th>来源</th><th>值</th><th>位置</th><th>字符串提示</th></tr></thead>
        <tbody>
          <tr v-for="(h, i) in swfDisplayHits" :key="i">
            <td><input type="checkbox" v-model="h.checked"></td>
            <td>{{ i + 1 }}</td>
            <td class="src">{{ h.source }}</td>
            <td class="val">{{ h.value }}</td>
            <td class="where">{{ h.where }}</td>
            <td class="hints">{{ (h.hints || []).join(', ') }}</td>
          </tr>
        </tbody>
      </table>
      </div>
    </div>

    <div class="card" v-show="playerVisible">
      <div class="hits-head">
        <span class="badge">Ruffle 试玩</span>
        <span class="status">rev={{ swfRev }}</span>
        <span class="spacer"></span>
        <button @click="playStatic">⟳ 载入最新补丁</button>
        <button @click="playerVisible = false">收起</button>
      </div>
      <div id="player-box" ref="playerBox"></div>
      <p class="dim-note">试玩基于 Ruffle（WebAssembly Flash 模拟器，经 CDN 加载，离线时不可用）。
      补丁后点「载入最新补丁」重开游戏即可看到效果。</p>
    </div>
  </section>

  <!-- ============ SOL 编辑器 ============ -->
  <section v-show="tab === 'sol'">
    <div class="upload-row">
      <label class="file-btn">📂 {{ solFile ? solFile.name : '选择 SOL 文件' }}<input type="file" accept=".sol" @change="e => solFile = e.target.files[0]"></label>
      <button class="primary" @click="uploadSol">上传解析</button>
      <button @click="loadDemo('sol')">试用内置 demo</button>
      <span class="status" :class="{ err: solErr }">{{ solStatus }}</span>
      <span class="spacer"></span>
      <input v-model="solSearch" placeholder="过滤路径 / 值…">
      <a v-if="solDownloadUrl" class="button" :href="solDownloadUrl">下载 .sol</a>
    </div>
    <div class="card tree">
      <tree-node v-if="solTree" name="(root)" :value="solTree" path="" :filter="solSearch" @edit="solEdit" />
    </div>
  </section>
</main>

<footer>仅用于本地单机游戏存档/资源的个人修改 · 补丁会话在服务重启后失效</footer>

<!-- 控制台抽屉 -->
<div :class="['drawer', { open: drawerOpen }]">
  <div class="drawer-head" @click="drawerOpen = !drawerOpen">
    <span class="badge">控制台</span>
    <span class="status">{{ modeText }}</span>
    <span class="spacer"></span>
    <button @click.stop="conLines = []">清空</button>
    <button @click.stop="drawerOpen = !drawerOpen">{{ drawerOpen ? '▾' : '▴' }}</button>
  </div>
  <div class="console-log" ref="conLog">
    <div v-for="(l, i) in conLines" :key="i" :class="'ln-' + l.cls">{{ l.text }}</div>
  </div>
  <div class="console-input">
    <span class="prompt">&gt;</span>
    <input v-model="conInput" :placeholder="conPlaceholder" autocomplete="off" spellcheck="false"
           @keydown.enter="exec" @keydown.up.prevent="histPrev" @keydown.down.prevent="histNext">
  </div>
</div>
<button class="fab" title="控制台" @click="drawerOpen = !drawerOpen">⌘</button>
<div class="toasts">
  <div v-for="t in toasts" :key="t.id" :class="['toast', t.type]">{{ t.text }}</div>
</div>
`;

const RT_HIT_CAP = 50000;
const SWF_DISPLAY_CAP = 200;

/* ---- 未知初值扫描存储（非响应式，紧凑类型化数组）----
   候选地址按块存于 Int32Array，last 存初值；缩小 = 原地压缩，
   千万级候选也能低内存快速过滤。rtTick 驱动界面刷新。 */
const rtState = { unknown: null, hits: null };

const app = createApp({
  template: TEMPLATE,
  components: { "tree-node": TreeNode },
  data() {
    return {
      tab: "rt",
      drawerOpen: false,
      toasts: [],
      _toastSeq: 0,
      conLines: [],
      conInput: "",
      conHistory: [],
      conHistPos: -1,

      // 运行时
      rtFile: null,
      rtPath: "",
      rtLoaded: false,
      rtName: "",
      rtMemCount: 0,
      rtMemMB: 0,
      rtStatus: "",
      rtErr: false,
      rtHits: null,
      rtNarrowMode: "exact",
      editCheatId: "",
      editVal: "",
      rtTick: 0,
      trainerOpen: true,
      stageFs: false,
      rtSearching: false,
      rtScanInfo: "",
      panelPos: null,
      panelDragging: false,
      rtLoading: false,
      rtLoadStage: "",
      rtLoadPct: null,
      rtLoadBytes: "",
      rtResWarn: "",
      rtSearchValue: "",
      rtSearchType: "auto",
      cheats: [],
      freshId: "",
      cfgId: "",
      valFocusId: "",
      ptTab: "search",
      rtObjectUrl: null,
      rtPlayer: null,

      // 静态补丁
      swfFile: null,
      swfId: "",
      swfName: "",
      swfRev: 0,
      swfSummary: "",
      swfModules: [],
      swfStatus: "",
      swfErr: false,
      swfHits: [],
      swfScanValue: "",
      swfScanMode: "any",
      swfNewValue: "",
      swfDownloadUrl: "",
      playerVisible: false,

      // SOL
      solFile: null,
      solId: "",
      solTree: null,
      solStatus: "",
      solErr: false,
      solSearch: "",
      solDownloadUrl: "",
    };
  },
  computed: {
    modeText() {
      void this.rtTick;
      if (this.tab === "rt") {
        return this.rtLoaded
          ? `运行时会话 · 命中 ${rtState.hits ? rtState.hits.length : "未扫描"}`
          : "运行时模式（先载入游戏）";
      }
      if (this.tab === "swf") {
        return this.swfId
          ? `静态补丁 ${this.swfId} · rev=${this.swfRev} · 命中 ${this.swfHits.length}`
          : "静态补丁模式（先上传 SWF）";
      }
      return this.solId ? `SOL 会话 ${this.solId}` : "SOL 模式（先上传存档）";
    },
    conPlaceholder() {
      if (this.tab === "rt") return "scan 100 · next 60 · patch 3 999999 · help";
      if (this.tab === "swf") return "help 查看命令 · scan 100 · patch 3 99999 · undo · play";
      return "help 查看命令 · get player.gold · set player.gold 999999";
    },
    hitSummary() {
      let t = `${this.swfHits.length} 处命中（勾选要修改的行）`;
      if (this.swfHits.length > SWF_DISPLAY_CAP) t += ` · 表格仅显示前 ${SWF_DISPLAY_CAP} 条，缩小范围后可全见`;
      return t;
    },
    swfDisplayHits() {
      return this.swfHits.slice(0, SWF_DISPLAY_CAP);
    },
    trainerStyle() {
      if (!this.stageFs) return null; // 窗口模式：普通卡片
      const s = {};
      if (this.panelPos) {
        s.left = this.panelPos.x + "px";
        s.top = this.panelPos.y + "px";
        s.right = "auto";
      }
      return s;
    },
    rtMemText() {
      return `已捕获 WASM 内存 ${this.rtMemCount} 块（${this.rtMemMB.toFixed(1)} MB）`;
    },
    rtHasHits() {
      void this.rtTick; // 未知初值扫描为非响应式存储，靠 tick 驱动刷新
      if (rtState.unknown) return rtState.unknown.total > 0;
      if (rtState.unknown) return rtState.unknown.total > 0;
      return rtState.hits !== null && rtState.hits.length > 0;
    },
    rtUnknownActive() {
      void this.rtTick;
      return !!rtState.unknown;
    },
    rtDisplayHits() {
      void this.rtTick;
      if (rtState.unknown) {
        const out = [];
        for (let ci = 0; ci < rtState.unknown.chunks.length && out.length < 100; ci++) {
          const ch = rtState.unknown.chunks[ci];
          const view = ch.type === "f64" ? new Float64Array(ch.mem.buffer) : new Int32Array(ch.mem.buffer);
          for (let k = 0; k < ch.n && out.length < 100; k++) {
            const addr = ch.addrs[k];
            const cur = ch.type === "f64" ? view[addr >> 3] : view[addr >> 2];
            const key = `c${ci}:${k}`;
            out.push({ key, type: ch.type, addr, cur, mem: ch.mem, memId: memIdOf(ch.mem) });
          }
        }
        return out;
      }
      const hits = rtState.hits || [];
      const dvs = new Map();
      return hits.slice(0, 100).map((h, i) => {
        let dv = dvs.get(h.mem);
        if (!dv) { dv = new DataView(h.mem.buffer); dvs.set(h.mem, dv); }
        const cur = h.type === "f64" ? dv.getFloat64(h.addr, true) : dv.getInt32(h.addr, true);
        return { key: "v" + i, type: h.type, addr: h.addr, cur, mem: h.mem, memId: memIdOf(h.mem) };
      });
    },
    searchBtnText() {
      void this.rtTick;
      if (this.rtSearching) return "搜索中…";
      if (rtState.unknown !== null || rtState.hits !== null) return "再次搜索";
      return this.rtSearchValue.trim() === "" ? "未知初值扫描" : "首次搜索";
    },
    rtCountText() {
      void this.rtTick;
      if (rtState.unknown) return `候选 ${rtState.unknown.total}`;
      if (rtState.hits !== null) return `命中 ${rtState.hits.length}`;
      return "";
    },
    rtResultText() {
      void this.rtTick;
      if (rtState.unknown) {
        let t = `未知初值候选 ${rtState.unknown.total} 处`;
        if (rtState.unknown.total > 100) t += "（显示前 100）";
        return t;
      }
      if (rtState.hits === null) return "尚未搜索";
      let t = `命中 ${rtState.hits.length} 处`;
      if (rtState.hits.length > 100) t += `（显示前 100）`;
      if (rtState.hits.length >= RT_HIT_CAP) t += " · 已达截断上限，建议用更独特的值";
      return t;
    },
  },
  methods: {
    /* ---- 控制台 ---- */
    notify(text, cls = "ok") {
      this.clog(text, cls);
      this.showToast(text, cls === "err" ? "err" : "ok");
    },
    showToast(text, type = "ok") {
      const id = ++this._toastSeq;
      this.toasts.push({ id, text, type });
      setTimeout(() => {
        this.toasts = this.toasts.filter(t => t.id !== id);
      }, 2600);
    },
    clog(text, cls = "") {
      this.conLines.push({ text, cls });
      nextTick(() => {
        const el = this.$refs.conLog;
        if (el) el.scrollTop = el.scrollHeight;
      });
    },
    exec() {
      const line = this.conInput.trim();
      if (!line) return;
      this.conInput = "";
      this.conHistory.push(line);
      this.conHistPos = this.conHistory.length;
      this.clog(line, "cmd");
      const parts = line.split(/\s+/);
      const cmd = parts[0].toLowerCase();
      const args = parts.slice(1);
      Promise.resolve(
        this.tab === "rt" ? this.execRt(cmd, args) : this.execTool(cmd, args),
      ).catch(e => this.clog("错误: " + e.message, "err"));
    },
    histPrev() {
      if (this.conHistPos > 0) {
        this.conHistPos--;
        this.conInput = this.conHistory[this.conHistPos] || "";
      }
    },
    histNext() {
      if (this.conHistPos < this.conHistory.length) {
        this.conHistPos++;
        this.conInput = this.conHistory[this.conHistPos] || "";
      }
    },

    /* ---- 内置 demo ---- */
    async loadDemo(kind) {
      try {
        if (kind === "sol") {
          const bytes = await (await fetch("/demo/demo.sol")).arrayBuffer();
          this.solFile = new File([bytes], "demo.sol", { type: "application/octet-stream" });
          await this.uploadSol();
          return;
        }
        const bytes = await (await fetch("/demo/game.swf")).arrayBuffer();
        const f = new File([bytes], "demo-game.swf", { type: "application/x-shockwave-flash" });
        if (kind === "swf") {
          this.swfFile = f;
          await this.uploadSwf();
        } else {
          this.rtFile = f;
          await this.rtLoad();
        }
      } catch (e) {
        this.clog("加载 demo 失败: " + e.message, "err");
      }
    },

    /* ---- 静态补丁 ---- */
    modText(m) {
      return m.error
        ? `DoABC[${m.index}] 解析失败: ${m.error}`
        : `DoABC[${m.index}] ${m.name || "(匿名)"} — 方法 ${m.methods} · 类 ${m.classes} · 脚本 ${m.scripts} · 方法体 ${m.bodies}`;
    },
    async uploadSwf() {
      const f = this.swfFile;
      if (!f) { this.swfErr = true; this.swfStatus = "请先选择 .swf 文件"; return; }
      this.swfErr = false;
      this.swfStatus = "解析中…";
      try {
        const res = await api("POST", "/api/swf", f, true);
        this.swfId = res.id;
        this.swfName = f.name;
        this.swfRev = 0;
        this.swfSummary = res.info.summary;
        this.swfModules = res.info.modules || [];
        this.swfHits = [];
        this.swfDownloadUrl = "";
        this.playerVisible = false;
        this.swfStatus = `${f.name} · ${res.info.summary}`;
        this.clog(`已载入 ${f.name}（${res.info.summary}）`, "ok");
      } catch (e) {
        this.swfErr = true;
        this.swfStatus = "失败: " + e.message;
      }
    },
    async doScan(refine) {
      if (!this.swfId) { this.swfErr = true; this.swfStatus = "请先上传 SWF"; return; }
      if (this.swfScanValue === "") { this.swfErr = true; this.swfStatus = "请输入要搜索的值"; return; }
      this.swfErr = false;
      try {
        const res = await api("POST", `/api/swf/${this.swfId}/scan`, {
          mode: this.swfScanMode,
          value: this.swfScanValue.trim(),
          refine,
        });
        this.swfHits = (res.hits || []).map(h => ({ ...h, checked: false }));
        this.clog(refine
          ? `二次扫描：缩小到 ${res.total} 处`
          : `扫描 "${this.swfScanValue}"：${res.total} 处命中`, "ok");
        this.swfHits.slice(0, 12).forEach(h =>
          this.clog(`#${h.index} ${h.source} = ${h.value} @ ${h.where}` +
            ((h.hints || []).length ? `  [${h.hints.join(",")}]` : "")));
        if (this.swfHits.length > 12) this.clog(`… 其余 ${this.swfHits.length - 12} 处见命中表格`, "dim");
      } catch (e) {
        this.swfErr = true;
        this.swfStatus = "失败: " + e.message;
      }
    },
    async applyChecked() {
      const indices = this.swfHits.map((h, i) => (h.checked ? i + 1 : 0)).filter(Boolean);
      if (!indices.length) { this.swfErr = true; this.swfStatus = "请先勾选要修改的命中"; return; }
      if (this.swfNewValue === "") { this.swfErr = true; this.swfStatus = "请输入新值"; return; }
      try {
        const res = await api("POST", `/api/swf/${this.swfId}/patch`, {
          indices, value: this.swfNewValue.trim(),
        });
        this.swfRev = res.rev;
        this.swfDownloadUrl = res.download;
        this.clog(`已补丁 ${res.patched} 处 → rev=${res.rev}。可试玩或下载`, "ok");
        // 用新值复扫：表格立即显示补丁后的命中（旧值消失即补丁生效的证明）
        this.swfScanValue = this.swfNewValue;
        await this.doScan(false);
      } catch (e) {
        this.swfErr = true;
        this.swfStatus = "失败: " + e.message;
      }
    },
    async playStatic() {
      if (!this.swfId) throw new Error("未加载 SWF");
      this.clog("启动 Ruffle 试玩…", "dim");
      await loadRuffle();
      this.playerVisible = true;
      await nextTick();
      const box = this.$refs.playerBox;
      box.innerHTML = "";
      const player = window.RufflePlayer.newest().createPlayer();
      box.appendChild(player);
      await player.load({ url: `/api/swf/${this.swfId}/raw?rev=${this.swfRev}`, allowScriptAccess: false });
      this.clog(`已载入 rev=${this.swfRev}。补丁后点「载入最新补丁」重开游戏。`, "ok");
    },

    /* ---- SOL ---- */
    async uploadSol() {
      const f = this.solFile;
      if (!f) { this.solErr = true; this.solStatus = "请先选择 .sol 文件"; return; }
      this.solErr = false;
      this.solStatus = "解析中…";
      try {
        const res = await api("POST", "/api/sol", f, true);
        this.solId = res.id;
        const tree = await api("GET", `/api/sol/${this.solId}`);
        this.solTree = tree;
        this.solDownloadUrl = `/api/sol/${this.solId}/download`;
        this.solStatus = `${f.name} · 存档名 "${res.name}"`;
        this.clog(`已载入存档 "${res.name}"，点击值即可修改`, "ok");
      } catch (e) {
        this.solErr = true;
        this.solStatus = "失败: " + e.message;
      }
    },
    async solEdit(payload) {
      try {
        const updated = await api("POST", `/api/sol/${this.solId}/set`, payload);
        this.solTree = updated;
        this.clog(`已修改 ${payload.path}`, "ok");
      } catch (e) {
        this.clog("失败: " + e.message, "err");
      }
    },

    /* ---- 运行时 ---- */
    onRtFilePicked(e) {
      this.rtFile = e.target.files[0] || null;
      if (this.rtFile) this.rtSmartLoad(); // 选完即载入
    },
    rtDrop(e) {
      const f = e.dataTransfer?.files?.[0];
      if (!f || !/\.swf$/i.test(f.name)) { this.clog("请拖入 .swf 文件", "dim"); return; }
      this.rtFile = f;
      this.clog(`收到拖入文件 ${f.name}`, "dim");
      this.rtSmartLoad();
    },
    /* 智能载入降级链：按名定位本地文件（完整资源支持）→ blob 兜底 */
    async rtSmartLoad() {
      const f = this.rtFile;
      if (!f) return;
      this.rtStatus = "定位文件…";
      try {
        const res = await api("POST", "/api/rt/byname", { name: f.name, size: f.size });
        if (res && res.found) {
          this.clog(`已自动定位本地文件：${res.path}`, "dim");
          const base = res.playUrl.slice(0, res.playUrl.lastIndexOf("/") + 1);
          await this.rtLoadFrom(res.playUrl, res.name, base, res.id, f.size);
          return;
        }
        this.clog("该文件尚未入库，以文件模式载入（若游戏白屏请粘贴其目录路径载入一次，之后就会自动识别）", "dim");
      } catch (e) { /* 索引不可用时直接降级 */ }
      await this.rtLoad();
    },
    async rtLoad() {
      const f = this.rtFile;
      if (!f) { this.rtErr = true; this.rtStatus = "请先选择 .swf 文件"; return; }
      this.rtErr = false;
      this.rtStatus = "载入 Ruffle…";
      this.clog(`载入游戏 ${f.name} …`, "dim");
      await loadRuffle();
      const buf = await f.arrayBuffer();
      if (this.rtObjectUrl) URL.revokeObjectURL(this.rtObjectUrl);
      this.rtObjectUrl = URL.createObjectURL(new Blob([buf], { type: "application/x-shockwave-flash" }));
      await this.rtLoadFrom(this.rtObjectUrl, f.name, "", "", f.size);
    },
    async loadMain() {
      if (this.rtPath.trim()) return this.rtOpenPath();
      if (this.rtFile) return this.rtLoad();
      this.rtErr = true;
      this.rtStatus = "请先选择文件或输入路径";
      return;
    },
    async rtOpenPath() {
      const p = this.rtPath.trim();
      if (!p) { this.rtErr = true; this.rtStatus = "请输入 SWF 路径"; return; }
      this.rtErr = false;
      this.rtStatus = "打开本地文件…";
      try {
        const res = await api("POST", "/api/rt/open", { path: p });
        localStorage.setItem("swfkit.rtPath", p); // 记住路径，下次自动填好
        this.clog(`已打开 ${res.name}（${(res.size / 1048576).toFixed(1)} MB），相对资源按其目录解析`, "dim");
        const base = res.playUrl.slice(0, res.playUrl.lastIndexOf("/") + 1);
        await this.rtLoadFrom(res.playUrl, res.name, base, res.id, res.size);
      } catch (e) {
        this.rtErr = true;
        this.rtStatus = "失败: " + e.message;
        this.clog("错误: " + e.message, "err");
      }
    },
    panelDragStart(e) {
      if (!this.stageFs) return; // 窗口模式无需拖动
      if (e.target.closest("button, input, select")) return;
      const el = this.$refs.trainerEl;
      const rect = el.getBoundingClientRect();
      this.panelPos = { x: rect.left, y: rect.top };
      const off = { x: e.clientX - rect.left, y: e.clientY - rect.top };
      this.panelDragging = true;
      const move = (ev) => {
        const w = el.offsetWidth, h = el.offsetHeight;
        this.panelPos = {
          x: Math.min(Math.max(ev.clientX - off.x, 0), window.innerWidth - w),
          y: Math.min(Math.max(ev.clientY - off.y, 0), window.innerHeight - h),
        };
      };
      const up = () => {
        this.panelDragging = false;
        window.removeEventListener("mousemove", move);
        window.removeEventListener("mouseup", up);
        localStorage.setItem("swfkit.panelPos", JSON.stringify(this.panelPos));
      };
      window.addEventListener("mousemove", move);
      window.addEventListener("mouseup", up);
    },
    async rtFullscreen() {
      const stage = this.$refs.stage;
      try {
        if (this.stageFs) {
          await document.exitFullscreen();
        } else {
          await stage.requestFullscreen(); // 整个舞台（游戏+悬浮训练器）进入全屏
        }
      } catch (e) {
        this.clog("全屏失败: " + e.message, "err");
      }
    },
    async rtLoadFrom(url, name, base, sessionId, expectedSize) {
      this.rtResWarn = "";
      this.rtLoading = true;
      this.rtLoadPct = null;
      this.rtLoadBytes = "";
      this.rtLoadStage = "加载模拟器…";
      await loadRuffle();
      this.rtLoadStage = "初始化播放器…";
      this.rtPlayer = window.RufflePlayer.newest().createPlayer();
      await nextTick();
      const box = this.$refs.rtBox;
      box.innerHTML = "";
      box.appendChild(this.rtPlayer);
      // base：AVM1 相对资源（loadMovie 等）的解析基准，真实播放器默认为 SWF 所在目录
      this.rtLoadStage = "读取游戏文件…";
      const stopPoll = sessionId && expectedSize ? this.rtStartProgressPoll(sessionId, expectedSize) : null;
      const minShow = new Promise(r => setTimeout(r, 1200)); // 遮罩最短显示，避免一闪而过
      const opts = base ? { url, base, allowScriptAccess: false } : { url, allowScriptAccess: false };
      await this.rtPlayer.load(opts);
      this.rtLoadStage = "启动游戏…";
      await new Promise(r => setTimeout(r, 600)); // 留出首帧渲染
      if (stopPoll) stopPoll();
      await minShow;
      this.rtLoadPct = 100;
      this.rtLoading = false;
      this.rtLoaded = true;
      rtState.hits = null;
      const mems = rtMemories();
      this.rtMemCount = mems.length;
      this.rtMemMB = mems.reduce((s, m) => s + m.buffer.byteLength, 0) / 1048576;
      this.rtName = name;
      this.rtStatus = `${name} 运行中`;
      this.clog(`游戏已运行：${name}。看到目标数值后：scan <值> → 游戏内改变它 → next <新值> → patch <序号> <值>`, "ok");
      // 资源加载失败检测：文件模式无法解析相对路径时给出明确指引
      setTimeout(() => this.rtCheckResError(), 4000);
      setTimeout(() => this.rtCheckResError(), 9000);
      // 自动重搜：游戏重载后自动扫描之前保存的数值
      const pending = this.cheats.filter(c => c.lastValue !== undefined);
      if (pending.length) {
        setTimeout(async () => {
          for (const c of pending) {
            this.rtSearchValue = String(c.lastValue);
            this.rtSearchType = c.type === 'i32' ? 'i32' : 'f64';
            await this.rtSmartSearch();
          }
          this.showToast(`已自动重搜 ${pending.length} 项`, "ok");
        }, 2000); // 等 Ruffle 稳定
      }
      // 自动重搜：游戏重载后自动扫描之前保存的数值
      const cheatsToRefind = this.cheats.filter(c => c.lastValue !== undefined);
      if (cheatsToRefind.length) {
        this.showToast("自动重搜之前保存的数值…", "ok");
        setTimeout(async () => {
          let found = 0;
          for (const c of cheatsToRefind) {
            p.rtSearchValue = String(c.lastValue);
            p.rtSearchType = c.type === 'i32' ? 'i32' : 'f64';
            const before = rtState.hits ? rtState.hits.length : 0;
            await p.rtSmartSearch();
            const now = rtState.hits ? rtState.hits.length : 0;
            if (now > 0) {
              // 更新地址
              if (now === 1) {
                c.addr = rtState.hits[0].addr;
                c.memId = rtState.hits[0].memId;
              }
              found++;
            }
          }
          this.showToast(`自动重搜完成：${found}/${cheatsToRefind.length} 项已重新定位`, "ok");
        }, 2000); // 等 Ruffle 稳定
      }
      // 修改表自动恢复（按游戏内容哈希）
      try {
        const buf = await (await fetch(url)).arrayBuffer();
        this.gameKey = fnvKey(new Uint8Array(buf));
        await this.loadCheatTable();
      } catch (e) { /* 表恢复失败不影响游戏 */ }
    },
    async loadCheatTable() {
      if (!this.gameKey) return;
      const res = await api("GET", `/api/table/load/${this.gameKey}`);
      if (!res.found || !Array.isArray(res.entries)) return;
      const mems = rtMemories();
      if (!mems.length) return;
      const mid = memIdOf(mems[0]);
      this.cheats = res.entries.map((e, i) => ({
        ...e,
        id: e.id || "r" + Date.now() + "_" + i + "_" + Math.floor(Math.random() * 100),
        memId: mid, cur: e.lock ? e.lockValue : null,
      }));
      if (this.cheats.some(c => c.lock)) startCheatEngine();
      this.clog(`已从修改表恢复 ${this.cheats.length} 条清单项`, "ok");
      this.showToast(`已恢复 ${this.cheats.length} 条修改项`, "ok");
    },
    _saveTimer: null,
    saveCheatsSoon() {
      clearTimeout(this._saveTimer);
      this._saveTimer = setTimeout(() => this.saveCheatsNow(), 500);
    },
    async saveCheatsNow() {
      if (!this.gameKey) return;
      const entries = this.cheats.map(c => ({
        id: c.id, desc: c.desc, type: c.type, addr: c.addr,
        lock: c.lock, lockValue: c.lockValue, delta: c.delta,
        hotkey: c.hotkey, hotkeyAdd: c.hotkeyAdd, hotkeyDec: c.hotkeyDec,
      }));
      await api("POST", "/api/table/save", { key: this.gameKey, name: this.rtName || "", entries });
    },
    // 轮询服务端传输统计：已服务字节 / 主文件大小 → 真实进度百分比
    rtStartProgressPoll(sessionId, expectedSize) {
      const tick = async () => {
        try {
          const res = await fetch(`/api/rt/progress/${sessionId}`);
          if (!res.ok) return;
          const d = await res.json();
          const pct = Math.min(100, Math.round(d.bytes * 100 / expectedSize));
          this.rtLoadPct = Math.max(this.rtLoadPct ?? 0, pct);
          this.rtLoadBytes = `${fmtSize(d.bytes)} / ${fmtSize(expectedSize)} · ${pct}%`;
        } catch (e) { /* 忽略轮询失败 */ }
      };
      tick();
      const h = setInterval(tick, 250);
      return () => clearInterval(h);
    },
    rtCheckResError() {
      if (!this.rtLoaded || this.rtResWarn) return;
      const bad = (window.__rtLog || []).find(l =>
        /Could not fetch|HttpNotOk|Asynchronous error/.test(l));
      if (bad) {
        this.rtResWarn = "检测到游戏外挂资源加载失败（当前为文件模式，无法解析相对路径）。" +
          "请把游戏文件所在目录的完整路径粘贴到输入框后点「载入」，即可完整运行；" +
          "此后同名文件会自动按此目录载入。";
        this.clog(this.rtResWarn, "err");
      }
    },
    rtStop() {
      if (this.rtObjectUrl) { URL.revokeObjectURL(this.rtObjectUrl); this.rtObjectUrl = null; }
      this.rtPlayer = null;
      this.$refs.rtBox.innerHTML = "";
      this.rtLoaded = false;
      rtState.hits = null;
      rtState.unknown = null;
      this.rtStatus = "";
      this.clog("运行时会话已结束", "dim");
    },
    rtRead(h) {
      try {
        const dv = new DataView(h.mem.buffer);
        return h.type === "f64" ? dv.getFloat64(h.addr, true) : dv.getInt32(h.addr, true);
      } catch (e) {
        return null; // 缓冲区已失效（内存扩容等）
      }
    },
    rtWrite(h, value) {
      const dv = new DataView(h.mem.buffer);
      if (h.type === "f64") dv.setFloat64(h.addr, value, true);
      else dv.setInt32(h.addr, value | 0, true);
    },
    rtFmtHit(h, i) {
      const v = rtRead(h);
      return `#${String(i + 1).padEnd(4)} ${h.type.padEnd(4)} @0x${h.addr.toString(16).padStart(8, "0")} = ${v === null ? "<不可读>" : v}`;
    },
    async rtScan(value, type) {
      const mems = rtMemories();
      if (!mems.length) throw new Error("未捕获到任何 WASM 内存（游戏尚未载入？）");
      const totalMB = mems.reduce((s, m) => s + m.buffer.byteLength, 0) / 1048576;
      this.clog(`扫描 ${mems.length} 块 WASM 内存（共 ${totalMB.toFixed(1)} MB），类型 ${type} …`, "dim");
      this.rtMemCount = mems.length;
      this.rtMemMB = totalMB;
      const t0 = performance.now();
      const hits = [];
      const push = (mem, addr, t) => {
        hits.push({ mem, addr, type: t, checked: false, last: value, cur: value });
        return hits.length >= RT_HIT_CAP;
      };
      const sleep0 = () => new Promise(r => setTimeout(r, 0));
      let doneMB = 0;
      const YIELD = 1 << 21; // 每 ~200 万槽位让出主线程并回报进度
      const prog = (mb) => {
        this.rtScanInfo = `已扫描 ${Math.round(doneMB + mb)} / ${Math.round(totalMB)} MB…`;
      };
      if (type === "f64" || type === "auto") {
        for (const m of mems) {
          // 8 字节对齐快速通道（Rust 枚举的 f64 载荷通常 8 对齐）
          const f = new Float64Array(m.buffer);
          for (let i = 0; i < f.length; i++) {
            const v = f[i];
            if (v === value && !Number.isNaN(v) && push(m, i * 8, "f64")) {
              this.clog(`命中过多，已截断到 ${RT_HIT_CAP}——先用更独特的值扫描`, "err");
              return hits;
            }
            if (i % YIELD === 0) { prog((i * 8) / 1048576); await sleep0(); }
          }
          await sleep0();
          // 4 字节对齐补扫（跳过已扫过的 8 对齐位置；DataView 允许非 8 对齐偏移）
          const dv = new DataView(m.buffer);
          for (let off = 4; off + 8 <= m.buffer.byteLength; off += 8) {
            const v = dv.getFloat64(off, true);
            if (v === value && !Number.isNaN(v) && push(m, off, "f64")) {
              this.clog(`命中过多，已截断到 ${RT_HIT_CAP}`, "err");
              return hits;
            }
          }
          await sleep0();
        }
      }
      if (type === "i32" || type === "auto") {
        for (const m of mems) {
          const a = new Int32Array(m.buffer);
          for (let i = 0; i < a.length; i++) {
            if (a[i] === value && push(m, i * 4, "i32")) {
              this.clog(`命中过多，已截断到 ${RT_HIT_CAP}`, "err");
              return hits;
            }
            if (i % YIELD === 0) { prog((i * 4) / 1048576); await sleep0(); }
          }
        }
      }
      this.rtScanInfo = "";
      this.clog(`耗时 ${((performance.now() - t0) / 1000).toFixed(2)}s，命中 ${hits.length} 处`, "ok");
      this.showToast(`扫描完成：命中 ${hits.length} 处`, "ok");
      return hits;
    },

    /* ---- 训练器表单（游戏修改大师式操作）---- */
    rtParseSearchValue() {
      const v = parseFloat(this.rtSearchValue);
      if (isNaN(v)) throw new Error(`无法解析数值 "${this.rtSearchValue}"`);
      return v;
    },
    /* CE 式智能搜索：没搜过 = 首次（有值精确 / 留空未知初值）；搜过 = 按下拉方式缩小 */
    async rtSmartSearch() {
      if (!this.rtLoaded) { this.rtErr = true; this.rtStatus = "请先载入游戏"; return; }
      const hasScan = rtState.unknown !== null || rtState.hits !== null;
      const raw = this.rtSearchValue.trim();
      this.rtErr = false;
      this.rtSearching = true;
      try {
        if (!hasScan) {
          if (raw === "") return await this.rtUnknownScan();
          const v = parseFloat(raw);
          if (isNaN(v)) { this.rtErr = true; this.rtStatus = `无法解析数值 "${raw}"`; return; }
          rtState.hits = await this.rtScan(v, this.rtSearchType);
          this.rtTick++;
          this.notify(`扫描完成：命中 ${rtState.hits.length} 处`);
          return;
        }
        const mode = this.rtNarrowMode;
        if (mode === "exact" && raw === "") {
          this.rtErr = true;
          this.rtStatus = "精确缩小需要输入数值；未知初值请选 变大了/变小了 等";
          return;
        }
        await this.rtNarrow(mode);
      } finally {
        this.rtSearching = false;
      }
    },
    /* 未知初值扫描：整块内存建立紧凑候选索引（f64 8 对齐 / i32 4 对齐） */
    async rtUnknownScan() {
      if (!this.rtLoaded) { this.rtErr = true; this.rtStatus = "请先载入游戏"; return; }
      this.rtErr = false;
      this.rtSearching = true;
      rtState.hits = null;
      rtState.unknown = null;
      await new Promise(r => setTimeout(r, 30)); // 让禁用态先渲染
      try {
        const type = this.rtSearchType === "i32" ? "i32" : "f64"; // auto → f64（AVM1 全数字、AS3 Number 均覆盖）
        const mems = rtMemories();
        if (!mems.length) throw new Error("未捕获到任何 WASM 内存");
        const chunks = [];
        let total = 0;
        const t0 = performance.now();
        const totalMB = mems.reduce((s, m2) => s + m2.buffer.byteLength, 0) / 1048576;
        let doneMB = 0;
        const YIELD_EVERY = 1 << 21; // 每 ~200 万槽位让出主线程并回报进度
        for (const m of mems) {
          const step = type === "f64" ? 8 : 4;
          const cap = Math.floor(m.buffer.byteLength / step);
          const addrs = new Int32Array(cap);
          const last = new Float64Array(cap);
          const view = type === "f64" ? new Float64Array(m.buffer) : new Int32Array(m.buffer);
          let n = 0;
          for (let i = 0; i < cap; i++) {
            const v = view[i];
            if (type === "f64" && Number.isNaN(v)) continue;
            addrs[n] = i * step;
            last[n] = v;
            n++;
            if (n % YIELD_EVERY === 0) {
              this.rtScanInfo = `已扫描 ${doneMB + (i * step) / 1048576 | 0} / ${totalMB | 0} MB…`;
              await new Promise(r => setTimeout(r, 0)); // 让出主线程，界面不冻
            }
          }
          doneMB += m.buffer.byteLength / 1048576;
          total += n;
          chunks.push({ mem: m, type, addrs, last, n });
          await new Promise(r => setTimeout(r, 0));
        }
        rtState.unknown = { type, chunks, total };
        this.clog(`未知初值扫描完成：候选 ${total} 处（${((performance.now() - t0) / 1000).toFixed(2)}s）。` +
          `改变目标数值后用 变大了/变小了/变了 缩小`, "ok");
      } catch (e) {
        this.rtErr = true;
        this.rtStatus = "失败: " + e.message;
      }
      this.rtSearching = false;
      this.rtScanInfo = "";
      this.rtTick++;
    },
    async rtNarrow(mode) {
      if (rtState.unknown) return await this.rtNarrowUnknown(mode);
      if (rtState.hits === null) return;
      let value = null;
      if (mode === "exact") {
        try {
          value = this.rtParseSearchValue();
        } catch (e) {
          this.rtErr = true;
          this.rtStatus = "失败: " + e.message;
          return;
        }
      }
      const before = rtState.hits.length;
      const dvs = new Map();
      const kept = rtState.hits.filter(h => {
        let dv = dvs.get(h.mem);
        if (!dv) { dv = new DataView(h.mem.buffer); dvs.set(h.mem, dv); }
        const cur = h.type === "f64" ? dv.getFloat64(h.addr, true) : dv.getInt32(h.addr, true);
        if (Number.isNaN(cur)) return false;
        h.cur = cur;
        if (mode === "exact") return cur === value;
        if (mode === "inc") return cur > h.last;
        if (mode === "dec") return cur < h.last;
        if (mode === "changed") return cur !== h.last;
        if (mode === "same") return cur === h.last;
        return true;
      });
      kept.forEach(h => { h.last = h.cur; });
      rtState.hits = kept;
      this.rtTick++;
      this.clog(`缩小：${before} → ${kept.length} 处`, "ok");
      this.notify(`缩小到 ${kept.length} 处`);
    },
    async rtNarrowUnknown(mode) {
      if (!rtState.unknown) return;
      let value = null;
      if (mode === "exact") {
        try {
          value = this.rtParseSearchValue();
        } catch (e) {
          this.rtErr = true;
          this.rtStatus = "失败: " + e.message;
          return;
        }
      }
      let before = rtState.unknown.total;
      let kept = 0;
      this.rtScanInfo = "过滤中…";
      for (const ch of rtState.unknown.chunks) {
        const view = ch.type === "f64" ? new Float64Array(ch.mem.buffer) : new Int32Array(ch.mem.buffer);
        let w = 0;
        for (let k = 0; k < ch.n; k++) {
          const addr = ch.addrs[k];
          const cur = ch.type === "f64" ? view[addr >> 3] : view[addr >> 2];
          if (Number.isNaN(cur)) continue;
          let keep;
          switch (mode) {
            case "exact": keep = cur === value; break;
            case "inc": keep = cur > ch.last[k]; break;
            case "dec": keep = cur < ch.last[k]; break;
            case "changed": keep = cur !== ch.last[k]; break;
            case "same": keep = cur === ch.last[k]; break;
          }
          if (keep) {
            ch.addrs[w] = addr;
            ch.last[w] = cur;
            w++;
          }
          if (w && (w % (1 << 20)) === 0) { // 每 ~100 万保留让出主线程
            this.rtScanInfo = `过滤中… 已保留 ${kept + w} 处`;
            await new Promise(r => setTimeout(r, 0));
          }
        }
        ch.n = w;
        kept += w;
      }
      rtState.unknown.total = kept;
      this.rtScanInfo = "";
      this.clog(`缩小：${before} → ${kept} 处（${{ exact: "精确", inc: "变大了", dec: "变小了", changed: "变了" }[mode] || mode}）`, "ok");
      this.rtTick++;
    },
    rtResetSearch() {
      rtState.hits = null;
      rtState.unknown = null;
      this.rtTick++;
      this.clog("已重置搜索", "dim");
    },
    async rtNarrowUnknown(mode) {
      if (!rtState.unknown) return;
      let value = null;
      if (mode === "exact") {
        try {
          value = this.rtParseSearchValue();
        } catch (e) {
          this.rtErr = true;
          this.rtStatus = "失败: " + e.message;
          return;
        }
      }
      let before = rtState.unknown.total;
      let kept = 0;
      this.rtScanInfo = "过滤中…";
      for (const ch of rtState.unknown.chunks) {
        const view = ch.type === "f64" ? new Float64Array(ch.mem.buffer) : new Int32Array(ch.mem.buffer);
        let w = 0;
        for (let k = 0; k < ch.n; k++) {
          const addr = ch.addrs[k];
          const cur = ch.type === "f64" ? view[addr >> 3] : view[addr >> 2];
          if (Number.isNaN(cur)) continue;
          let keep;
          switch (mode) {
            case "exact": keep = cur === value; break;
            case "inc": keep = cur > ch.last[k]; break;
            case "dec": keep = cur < ch.last[k]; break;
            case "changed": keep = cur !== ch.last[k]; break;
            case "same": keep = cur === ch.last[k]; break;
          }
          if (keep) {
            ch.addrs[w] = addr;
            ch.last[w] = cur;
            w++;
          }
          if (w && (w % (1 << 20)) === 0) { // 每 ~100 万保留让出主线程
            this.rtScanInfo = `过滤中… 已保留 ${kept + w} 处`;
            await new Promise(r => setTimeout(r, 0));
          }
        }
        ch.n = w;
        kept += w;
      }
      rtState.unknown.total = kept;
      this.rtScanInfo = "";
      this.clog(`缩小：${before} → ${kept} 处（${{ exact: "精确", inc: "变大了", dec: "变小了", changed: "变了" }[mode] || mode}）`, "ok");
      this.rtTick++;
    },
    rtResetSearch() {
      rtState.hits = null;
      rtState.unknown = null;
      this.rtTick++;
      this.clog("已重置搜索", "dim");
    },

    /* ---- 指针追踪 ---- */
    async ptrTrack(h) {
      const m = memById(h.memId);
      if (!m) { this.clog("地址已失效", "err"); return; }
      this.rtScanInfo = "指针追踪…";
      const chains = await rtPointerScan(m, h.addr, {
        onProgress: msg => { this.rtScanInfo = msg; },
      });
      this.rtScanInfo = "";
      // 合并所有链，优先二级（跨重启更稳定），同级取最小根槽位
      const all = [...chains.depth2, ...chains.depth1];
      if (!all.length) { this.clog("未找到可用指针链", "err"); return; }
      all.sort((a, b) => a.slots.length - b.slots.length || a.slots[0] - b.slots[0]);
      const best = all[0];

      // ★ 只保留一条：替换该地址的旧条目（含裸地址条目），加入最优指针链
      const oldRaw = this.cheats.find(c => c.addr === h.addr && c.type !== "ptr");
      if (oldRaw) {
        best.desc = oldRaw.desc || `0x${h.addr.toString(16)}`;
        best.delta = oldRaw.delta || 100;
        best.hotkey = oldRaw.hotkey || ""; best.hotkeyAdd = oldRaw.hotkeyAdd || ""; best.hotkeyDec = oldRaw.hotkeyDec || "";
        best.lock = oldRaw.lock || false; best.lockValue = oldRaw.lockValue || 0;
        this.cheats = this.cheats.filter(c => c !== oldRaw);
      }
      const id = "p" + Date.now() + "_" + Math.floor(Math.random() * 1000);
      this.cheats.push({
        id,
        desc: best.desc || `0x${h.addr.toString(16)}`,
        memId: h.memId, type: "ptr", chain: best,
        cur: chainRead(m, best), lock: best.lock, lockValue: best.lockValue,
        delta: best.delta, hotkey: best.hotkey, hotkeyAdd: best.hotkeyAdd, hotkeyDec: best.hotkeyDec,
      });
      startCheatEngine();
      this.saveCheatsSoon();
      this.ptTab = "cheats";
      this.notify(`已建立指针追踪（${best.slots.length} 级链）——重启游戏后自动追踪 ✓`);
    },

    /* ---- 修改清单（CE 式地址清单 + 锁定 + 热键）---- */
    addCheatFromHit(h) {
      const cur = rtRead(h);
      if (cur === null) { this.clog("该地址已失效，无法加入清单", "err"); return; }
      const id = "c" + Date.now() + "_" + Math.floor(Math.random() * 1000);
      this.cheats.push({
        id,
        desc: `0x${h.addr.toString(16)}`,
        memId: h.memId, addr: h.addr, type: h.type,
        cur, lock: false, lockValue: cur,
        delta: 100, hotkey: "", hotkeyAdd: "", hotkeyDec: "",
      });
      startCheatEngine();
      this.ptTab = "cheats";
      this.showToast(`已加入清单：0x${h.addr.toString(16)}`, "ok");
      this.freshId = id;
      setTimeout(() => { if (this.freshId === id) this.freshId = ""; }, 1800);
      this.clog(`已加入清单：0x${h.addr.toString(16)}（当前 ${cur}）`, "ok");
      // 异步指针扫描：找到稳定链后自动升级为指针追踪（跨重启有效）
      const m = memById(h.memId);
      if (m) {
        rtPointerScan(m, h.addr, { maxDepth: 1, onProgress: () => {} }).then(chains => {
          const best = chains.depth1[0]; // 最小槽位地址 = 最早分配 = 最稳定
          if (!best) return;
          const entry = this.cheats.find(c2 => c2.id === id);
          if (entry) {
            entry.type = "ptr";
            entry.chain = best;
            this.showToast("已建立指针追踪，重启游戏后自动追踪 ✓", "ok");
            this.saveCheatsSoon();
          }
        });
      }
    },
    delCheat(id) {
      this.cheats = this.cheats.filter(c => c.id !== id);
      this.saveCheatsSoon();
    },
    toggleLock(c) {
      if (!c.lock) {
        const cur = cheatRead(c);
        if (cur === null) { this.clog("地址已失效，无法锁定", "err"); return; }
        c.lockValue = cur;
        c.lock = true;
        startCheatEngine();
        this.clog(`已锁定 ${c.desc || c.addr} = ${cur}（每 150ms 写回）`, "ok");
      } else {
        c.lock = false;
        this.saveCheatsSoon();
        this.notify(`已解锁 ${c.desc || c.addr}`);
      }
    },
    adjustCheat(c, d) {
      const cur = cheatRead(c);
      if (cur === null) { this.clog("地址已失效", "err"); return; }
      const nv = cur + (Number(d) || 0);
      if (cheatWrite(c, nv)) {
        c.cur = nv;
        if (c.lock) c.lockValue = nv; // 锁定状态下 ±同步冻结值
        this.notify(`${c.desc || "清单项"}：${cur} → ${nv}`);
      } else {
        this.clog("地址已失效", "err");
      }
    },
    setHk(c, field, e) {
      e.preventDefault();
      e.stopPropagation();
      if (e.key === "Escape") { c[field] = ""; return; }
      c[field] = e.code; // 如 Numpad1 / F8 / KeyQ
      this.saveCheatsSoon();
      this.clog(`热键已设置：${c.desc || "清单项"} ${field === "hotkey" ? "锁定开关" : field === "hotkeyAdd" ? "增加" : "减少"} = ${e.code}`, "ok");
    },

    /* ---- 清单值输入：失焦/回车写内存 ---- */
    writeCheatVal(c, e) {
      this.valFocusId = "";
      const v = parseFloat(e.target.value);
      if (isNaN(v)) { e.target.value = String(c.cur); return; } // 非法输入还原
      if (cheatWrite(c, v)) {
        c.cur = v;
        if (c.lock) c.lockValue = v;
        this.saveCheatsSoon();
        this.notify(`${c.desc || "清单项"} → ${v}${c.lock ? "（已锁定）" : ""}`);
      } else {
        e.target.value = String(c.cur);
        this.clog("地址已失效", "err");
      }
    },

    /* ---- 控制台命令分发 ---- */
    async execRt(cmd, args) {
      switch (cmd) {
        case "help":
          [
            "── 运行时修改（需先载入游戏）──",
            "  scan <值> [f64|i32|auto]   全内存扫描（默认 f64，AS3 Number 最常见）",
            "  uscan [i32]                未知初值扫描（血条/蓝条等看不到精确值时用）",
            "  next <值>                  在既有命中中缩小（游戏里先改变数值再执行）",
            "  hits                       列出当前命中（实时重读内存值）",
            "  patch <序号|all> <值>      直接写内存，游戏立即生效；all 需谨慎",
            "  info                       内存与会话状态",
            "  stop                       结束运行时会话",
          ].forEach(l => this.clog(l, "dim"));
          return;
        case "info": {
          const mems = rtMemories();
          const totalMB = mems.reduce((s, m) => s + m.buffer.byteLength, 0) / 1048576;
          this.clog(`内存块 ${mems.length}（${totalMB.toFixed(1)} MB）· 命中 ${rtState.hits ? rtState.hits.length : "未扫描"} · Ruffle ${this.rtLoaded ? "运行中" : "未载入"}`, "ok");
          return;
        }
        case "stop":
          this.rtStop();
          return;
        case "uscan":
          this.rtSearchType = args[0] === "i32" ? "i32" : (args[0] === "f64" ? "f64" : "auto");
          await this.rtUnknownScan();
          return;
        case "scan":
        case "next": {
          if (!this.rtLoaded) throw new Error("游戏未载入，请先在运行时页载入 .swf");
          if (!args.length) throw new Error("用法: " + cmd + " <值> [f64|i32|auto]");
          const value = parseFloat(args[0]);
          if (isNaN(value)) throw new Error(`无法解析数值 "${args[0]}"`);
          const type = (args[1] || "f64").toLowerCase();
          if (cmd === "next") {
            if (rtState.unknown) return await this.rtNarrowUnknown("exact");
            if (rtState.hits === null) throw new Error("还没有初始扫描（先 scan）");
            const before = rtState.hits.length;
            const dvs = new Map();
            rtState.hits = rtState.hits.filter(h => {
              let dv2 = dvs.get(h.mem);
              if (!dv2) dv2 = dvs.set(h.mem, new DataView(h.mem.buffer)).get(h.mem);
              const cur = h.type === "f64" ? dv2.getFloat64(h.addr, true) : dv2.getInt32(h.addr, true);
              h.cur = cur; h.last = cur;
              return cur === value;
            });
            this.clog(`二次扫描：${before} → ${rtState.hits.length} 处`, "ok");
          } else {
            rtState.unknown = null;
            rtState.hits = await this.rtScan(value, type);
          }
          rtState.hits.slice(0, 20).forEach((h, i) => this.clog(this.rtFmtHit(h, i)));
          if (rtState.hits.length > 20) this.clog(`… 其余 ${rtState.hits.length - 20} 处用 hits 查看`, "dim");
          return;
        }
        case "hits": {
          if (!this.rtLoaded) throw new Error("游戏未载入");
          const hs = rtState.hits || [];
          if (!hs.length) { this.clog("当前无命中（先 scan）", "dim"); return; }
          hs.slice(0, 100).forEach((h, i) => this.clog(this.rtFmtHit(h, i)));
          if (hs.length > 100) this.clog(`… 其余 ${hs.length - 100} 处`, "dim");
          return;
        }
        case "patch": {
          if (!this.rtLoaded) throw new Error("游戏未载入");
          if (args.length < 2) throw new Error("用法: patch <序号|all> <值>");
          const sel = args[0].toLowerCase();
          const value = parseFloat(args[1]);
          if (isNaN(value)) throw new Error(`无法解析数值 "${args[1]}"`);
          let ok = 0;
          if (rtState.unknown) {
            // 未知初值模式：序号对应压缩后的候选顺序
            const targets = sel === "all"
              ? rtState.unknown.chunks.map((_, i) => i)
              : sel.split(",").map(n => parseInt(n, 10) - 1);
            let gi = 0;
            for (let ci = 0; ci < rtState.unknown.chunks.length; ci++) {
              const ch = rtState.unknown.chunks[ci];
              const view = ch.type === "f64" ? new Float64Array(ch.mem.buffer) : new Int32Array(ch.mem.buffer);
              for (let k = 0; k < ch.n; k++) {
                const nth = targets.indexOf(gi);
                if (nth >= 0) {
                  const addr = ch.addrs[k];
                  if (ch.type === "f64") view[addr >> 3] = value;
                  else view[addr >> 2] = value | 0;
                  ch.last[k] = value;
                  ok++;
                }
                gi++;
              }
            }
          } else {
            if (rtState.hits === null || !rtState.hits.length) throw new Error("当前无命中（先 scan）");
            const targets = sel === "all"
              ? rtState.hits.map((_, i) => i)
              : sel.split(",").map(n => parseInt(n, 10) - 1);
            if (targets.some(n => isNaN(n) || n < 0 || n >= rtState.hits.length)) {
              throw new Error("序号超出范围");
            }
            for (const n of targets) {
              if (rtRead(rtState.hits[n]) === null) continue;
              rtWrite(rtState.hits[n], value);
              rtState.hits[n].cur = value;
              rtState.hits[n].last = value;
              ok++;
            }
          }
          this.clog(`已写入 ${ok} 处 → ${value}。回游戏看效果；如果游戏崩了刷新页面重载即可`, "ok");
          return;
        }
        default:
          throw new Error(`未知命令 "${cmd}"，输入 help 查看运行时命令`);
      }
    },
    async execTool(cmd, args) {
      switch (cmd) {
        case "help":
          [
            "── 静态补丁（SWF 标签页）──",
            "  scan <值> [any|int|uint|double]   全量扫描数值",
            "  next <值>                         在上次命中中二次扫描缩小范围",
            "  hits                              列出当前命中",
            "  patch <序号|序号组|all> <新值> [归因过滤]",
            "  undo / play / download / info",
            "── SOL 存档（SOL 标签页）──",
            "  get <路径> / set <路径> <值> / save",
          ].forEach(l => this.clog(l, "dim"));
          return;
        case "scan":
        case "next": {
          if (!this.swfId) throw new Error("未加载 SWF，请先在静态补丁页上传");
          if (!args.length) throw new Error("用法: " + cmd + " <值> [any|int|uint|double]");
          this.swfScanValue = args[0];
          this.swfScanMode = args[1] || "any";
          const before = this.swfHits.length;
          await this.doScan(cmd === "next");
          if (cmd === "next") this.clog(`（于上次 ${before} 处中缩小）`, "dim");
          return;
        }
        case "hits": {
          if (!this.swfHits.length) { this.clog("当前无命中（先 scan）", "dim"); return; }
          this.swfHits.forEach((h, i) =>
            this.clog(`#${i + 1} ${h.source} = ${h.value} @ ${h.where}`));
          return;
        }
        case "patch": {
          if (!this.swfId) throw new Error("未加载 SWF");
          if (args.length < 2) throw new Error("用法: patch <序号|序号组|all> <新值> [归因过滤]");
          const sel = args[0].toLowerCase();
          const value = args[1];
          const where = args.slice(2).join(" ");
          let pool = this.swfHits;
          if (where) pool = pool.filter(h => h.where.includes(where));
          let indices;
          if (sel === "all") {
            indices = pool.map((_, i) => i + 1);
          } else {
            indices = sel.split(",").map(n => parseInt(n, 10));
          }
          if (!indices.length) throw new Error("没有匹配的命中");
          const res = await api("POST", `/api/swf/${this.swfId}/patch`, { indices, value });
          this.swfRev = res.rev;
          this.swfDownloadUrl = res.download;
          this.clog(`已补丁 ${res.patched} 处 → rev=${res.rev}。play 试玩 / download 下载`, "ok");
          return;
        }
        case "undo": {
          if (!this.swfId) throw new Error("未加载 SWF");
          const res = await api("POST", `/api/swf/${this.swfId}/undo`, {});
          this.swfRev = res.rev;
          this.swfHits = [];
          this.clog(`已回滚到 rev=${res.rev}，命中列表已清空`, "ok");
          return;
        }
        case "play":
          await this.playStatic();
          return;
        case "download":
          if (!this.swfId) throw new Error("未加载 SWF");
          this.clog(`下载链接: /api/swf/${this.swfId}/download`, "ok");
          return;
        case "info":
          if (!this.swfId) throw new Error("未加载 SWF");
          this.clog(`会话 ${this.swfId} · rev=${this.swfRev} · 命中 ${rtState.hits ? rtState.hits.length : 0} 处`, "ok");
          return;
        case "get": {
          if (!this.solId) throw new Error("未加载 SOL");
          if (!args.length) throw new Error("用法: get <路径>");
          const res = await api("POST", `/api/sol/${this.solId}/get`, { path: args[0] });
          if (!res.found) { this.clog(`${args[0]} 不存在`, "err"); return; }
          this.clog(`${res.path} = ${JSON.stringify(res.value)}`, "ok");
          return;
        }
        case "set": {
          if (!this.solId) throw new Error("未加载 SOL");
          if (args.length < 2) throw new Error("用法: set <路径> <值>");
          await this.solEdit({ path: args[0], value: args.slice(1).join(" ") });
          return;
        }
        case "save":
          if (!this.solId) throw new Error("未加载 SOL");
          this.clog(`下载链接: /api/sol/${this.solId}/download`, "ok");
          return;
        default:
          throw new Error(`未知命令 "${cmd}"，输入 help 查看列表`);
      }
    },
  },
  mounted() {
    this.clog("控制台就绪。载入文件后输入 help 查看命令。", "dim");
    this.rtPath = localStorage.getItem("swfkit.rtPath") || "";
    try { this.panelPos = JSON.parse(localStorage.getItem("swfkit.panelPos") || "null"); } catch (e) {}
    document.addEventListener("fullscreenchange", () => {
      this.stageFs = !!document.fullscreenElement;
    });
  },
});

/* ---- 冻结引擎：150ms 写回锁定值；500ms 刷新清单当前值 ---- */
let cheatEngineTimer = null;
function startCheatEngine() {
  if (cheatEngineTimer) return;
  cheatEngineTimer = setInterval(() => {
    const p = window.swfkit;
    if (!p) return;
    for (const c of p.cheats) {
      if (c.lock) cheatWrite(c, c.lockValue);
    }
  }, 150);
  // 清单当前值刷新
  setInterval(() => {
    const p2 = window.swfkit;
    if (!p2) return;
    let dirty = false;
    for (const c of p2.cheats) {
      if (p2.valFocusId === c.id) continue; // 用户正在编辑，不覆盖
      const v = cheatRead(c);
      if (v !== null && !c.lock && v !== c.cur) { c.cur = v; dirty = true; }
      else if (v === null && c.cur !== null) { c.cur = null; dirty = true; }
    }
    if (dirty) p2.rtTick++;
  }, 500);
}

/* ---- 全局热键分发（捕获阶段，全屏/游戏聚焦均生效）---- */
window.addEventListener("keydown", (e) => {
  const p = window.swfkit;
  if (!p || !p.cheats.length) return;
  for (const c of p.cheats) {
    if (c.hotkey && c.hotkey === e.code) {
      p.toggleLock(c);
      e.preventDefault();
      return;
    }
    if (c.hotkeyAdd && c.hotkeyAdd === e.code) {
      p.adjustCheat(c, Number(c.delta) || 0);
      e.preventDefault();
      return;
    }
    if (c.hotkeyDec && c.hotkeyDec === e.code) {
      p.adjustCheat(c, -(Number(c.delta) || 0));
      e.preventDefault();
      return;
    }
  }
}, true);

app.directive("focus", {
  mounted(el) {
    el.focus();
    el.select();
  },
});

app.component("tree-node", TreeNode);
// 挂载并把组件代理暴露为调试句柄（控制台/自动化测试可用：swfkit.xxx）
window.swfkit = app.mount("#app");

/* ---- 运行时内存工具（非响应式，直接挂在全局）---- */
function rtMemories() {
  return [...(window.__wasmMemories || [])].filter(m => m.buffer && m.buffer.byteLength > 0);
}
function rtRead(h) {
  try {
    const dv = new DataView(h.mem.buffer);
    return h.type === "f64" ? dv.getFloat64(h.addr, true) : dv.getInt32(h.addr, true);
  } catch (e) {
    return null; // 缓冲区已失效（内存扩容等）
  }
}
function rtWrite(h, value) {
  const dv = new DataView(h.mem.buffer);
  if (h.type === "f64") dv.setFloat64(h.addr, value, true);
  else dv.setInt32(h.addr, value | 0, true);
}
