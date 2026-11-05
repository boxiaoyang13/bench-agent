#!/usr/bin/env python3
"""THVV Server 路径寻址的参考实现（normative reference）.

Go 侧的 `internal/config.Resolver` 必须与这里行为一致。用于：
  1. 跑 `-check` 时的期望输出
  2. 单元测试的 golden 用例
  3. 文档评审时肉眼验证前缀解析是否符合预期

规则（见 CONFIG.md §3）:
  - roots 先解析（可引用其他根，按声明顺序，禁止循环）
  - 路径字段 6 种写法：<root>:<rel> / 绝对路径 / 无前缀相对（相对配置文件目录）
  - 只按第一个 ':' 切分；单字母前缀视为 Windows 盘符 → 绝对路径
  - <rel> 不允许 ../ 逃逸出根
  - '.' 或空 = 该根本身
  - 支持 ~ 展开

自测： python3 path_resolver_reference.py thvv.yaml
"""
from __future__ import annotations

import os
import re
import sys
from pathlib import Path

RESERVED = {"thvv", "data", "bin", "cfg"}
PREFIX_RE = re.compile(r"^([A-Za-z][A-Za-z0-9_-]*):(.*)$")
ENV_RE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}")


class ResolveError(Exception):
    pass


def expand_env(value: str, env: dict[str, str], field: str) -> str:
    """展开 ${VAR} / ${VAR:-default}，只做一轮。"""
    missing: list[str] = []

    def sub(m: re.Match) -> str:
        name, default = m.group(1), m.group(2)
        if name in env and env[name] != "":
            return env[name]
        if default is not None:
            return default
        missing.append(name)
        return m.group(0)

    out = ENV_RE.sub(sub, value)
    if missing:
        raise ResolveError(f"{field}: 未设置环境变量 {', '.join(sorted(set(missing)))}")
    return out


class Resolver:
    def __init__(self, config_dir: str | Path, roots: dict[str, str], env: dict[str, str] | None = None):
        self.config_dir = Path(config_dir).resolve()
        self.env = dict(os.environ) if env is None else env
        self.roots: dict[str, Path] = {"cfg": self.config_dir}   # cfg 是隐式内置根
        # thvv / data / bin 按声明顺序解析（bin 的默认值引用 thvv）
        for name in ("thvv", "data", *[k for k in roots if k not in ("thvv", "data", "cfg")]):
            if name in roots:
                self.roots[name] = self._resolve_root(name, roots[name], roots)
        if "bin" not in self.roots and "bin" in roots:
            self.roots["bin"] = self._resolve_root("bin", roots["bin"], roots)

    # ---- 根解析：允许 root 值引用其他（已解析的）根 -------------------------
    def _resolve_root(self, name: str, raw: str, all_roots: dict[str, str], stack: tuple = ()) -> Path:
        if name in stack:
            raise ResolveError(f"roots: cycle detected {' → '.join(stack + (name,))}")
        expanded = expand_env(str(raw), self.env, f"roots.{name}")
        # 根的值本身可以是 <other_root>:rel（如 bin: "thvv:.venv/bin"）
        m = PREFIX_RE.match(expanded)
        if m and len(m.group(1)) > 1 and m.group(1) in all_roots:
            base = self._resolve_root(m.group(1), all_roots[m.group(1)], all_roots, stack + (name,))
            return self._clean(base / m.group(2))
        return self._clean(self._expand_home(expanded), base=self.config_dir)

    # ---- runtime.env 的值：宽松规则 ----------------------------------------
    def resolve_env_value(self, value: str) -> str:
        """env 值只在命中「已知根前缀」时才解析，否则视为字面量（如 "1" / "false"）。"""
        expanded = expand_env(str(value), self.env, "env")
        m = PREFIX_RE.match(expanded)
        if m and len(m.group(1)) > 1 and m.group(1) in self.roots:
            return str(self.resolve(expanded, "env"))
        if expanded.startswith(("~", "/")):
            return str(self.resolve(expanded, "env"))
        return expanded

    # ---- 普通路径解析 ------------------------------------------------------
    def resolve(self, value: str, field: str = "<path>") -> Path:
        if value is None or value == "":
            raise ResolveError(f"{field}: 空路径")
        expanded = expand_env(str(value), self.env, field)
        m = PREFIX_RE.match(expanded)
        if m and len(m.group(1)) > 1:                 # 单字母 → Windows 盘符，走绝对路径
            prefix, rel = m.group(1), m.group(2)
            if prefix not in self.roots:
                raise ResolveError(f"{field}: 未知根 {prefix!r}，可用: {', '.join(sorted(self.roots))}")
            base = self.roots[prefix]
            target = self._clean(base / rel if rel else base)
            if not self._inside(target, base):
                raise ResolveError(f"{field}: 路径逃逸出根 {prefix!r} → {target}")
            return target
        return self._clean(self._expand_home(expanded), base=self.config_dir)

    # ---- helpers -----------------------------------------------------------
    @staticmethod
    def _expand_home(p: str) -> str:
        return os.path.expanduser(p)

    @staticmethod
    def _clean(p: Path, base: Path | None = None) -> Path:
        p = Path(p)
        if base is not None and not p.is_absolute():
            p = base / p
        return Path(os.path.normpath(p))

    @staticmethod
    def _inside(target: Path, base: Path) -> bool:
        try:
            target.relative_to(base)
            return True
        except ValueError:
            return False


def collect_paths(cfg: dict) -> list[tuple[str, str]]:
    """从配置中收集所有路径类字段（与 Go 侧字段清单保持一致）。"""
    out: list[tuple[str, str]] = []
    for k, v in (cfg.get("paths") or {}).items():
        out.append((f"paths.{k}", v))
    store = cfg.get("store") or {}
    if (sq := store.get("sqlite")) and sq.get("path"):
        out.append(("store.sqlite.path", sq["path"]))
    if (sec := cfg.get("secrets") or {}).get("provider") == "file":
        out.append(("secrets.file.path", sec["file"]["path"]))
    if (ui := (cfg.get("server") or {}).get("ui")) and ui.get("dir"):
        out.append(("server.ui.dir", ui["dir"]))
    for ename, eng in (cfg.get("engines") or {}).items():
        rt = eng.get("runtime") or {}
        for k in ("home", "python", "evalscope"):
            if rt.get(k):
                out.append((f"engines.{ename}.runtime.{k}", rt[k]))
        for pp in rt.get("pythonpath_prepend") or []:
            out.append((f"engines.{ename}.runtime.pythonpath_prepend", pp))
        for ek, ev in (rt.get("env") or {}).items():
            out.append((f"#env engines.{ename}.runtime.env.{ek}", ev))
        for cname, cap in (eng.get("capabilities") or {}).items():
            for k in ("working_dir", "script", "plan_schema", "result_schema"):
                if cap.get(k):
                    out.append((f"engines.{ename}.{cname}.{k}", cap[k]))
            crt = cap.get("runtime") or {}
            for k in ("home", "python", "evalscope"):
                if crt.get(k):
                    out.append((f"engines.{ename}.{cname}.runtime.{k}", crt[k]))
    return out


def main() -> int:
    if len(sys.argv) < 2:
        print(__doc__)
        return 2
    import yaml  # 仅本脚本需要；Go 侧用 yaml.v3

    cfg_path = Path(sys.argv[1]).resolve()
    cfg = yaml.safe_load(cfg_path.read_text(encoding="utf-8"))
    roots = cfg.get("roots") or {}
    if not (roots.get("thvv") or os.environ.get("THVV_HOME")):
        print("roots.thvv: required（或 export THVV_HOME）", file=sys.stderr)
        return 1
    roots.setdefault("thvv", "${THVV_HOME}")
    roots.setdefault("data", "./data")

    r = Resolver(cfg_path.parent, roots)
    print(f"config: {cfg_path}")
    for name, base in r.roots.items():
        exists = "exists" if base.exists() else "MISSING"
        print(f"[root] {name:<10} = {base}  ({exists})")

    data_root = r.roots.get("data")
    bad = 0
    for field, raw in collect_paths(cfg):
        # env 值走宽松规则（非路径字面量不报错）
        if field.startswith("#env "):
            print(f"[env ] {field[5:]:<48} {raw:<34} → {r.resolve_env_value(raw)}")
            continue
        try:
            p = r.resolve(raw, field)
            under_data = data_root is not None and str(p).startswith(str(data_root))
            if p.exists():
                tag = "exists"
            elif under_data:
                tag = "will create"      # data 根下的目录由服务自行创建
            else:
                tag = "MISSING"
                bad += 1
            print(f"[path] {field:<48} {raw:<34} → {p}  ({tag})")
        except ResolveError as e:
            bad += 1
            print(f"[path] {field:<48} {raw:<34} → ERROR: {e}")

    print(f"\n未命中 {bad} 项")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
