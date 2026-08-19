/**
 * 校验所有 i18n 文案都能被 vue-i18n 的消息编译器成功编译。
 *
 * 存在的理由：文案里出现裸露的 `@` 会被当作 linked message 语法（`@:key`）解析，
 * 编译期抛 "Invalid linked format" 并让整个页面白屏。而这类崩溃有两个特点使它
 * 极易漏到线上：
 *   1. 只在该条消息**首次被实际渲染**时才编译，藏在需要交互才出现的
 *      placeholder / 弹窗文案里就不会在首屏暴露；
 *   2. vitest 默认解析到 vue-i18n 的 runtime-only 构建，压根不做编译，
 *      所以挂载组件的测试也照样通过。
 *
 * 所以这里显式引入 @intlify/message-compiler 来编译，绕开上面第 2 点。
 * 字面量 `@` 需按仓库既有约定写成 `{'@'}`（见 smtp / proxy 等文案）。
 */
import { describe, expect, it } from "vitest";
import { baseCompile } from "@intlify/message-compiler";
import zh from "@/i18n/locales/zh";
import en from "@/i18n/locales/en";

type Node = Record<string, unknown> | string | unknown[];

/** 把嵌套文案对象拍平成 [路径, 文案] 列表 */
function flatten(node: Node, prefix = ""): [string, string][] {
  if (typeof node === "string") return [[prefix, node]];
  if (Array.isArray(node)) {
    return node.flatMap((v, i) => flatten(v as Node, `${prefix}[${i}]`));
  }
  if (node && typeof node === "object") {
    return Object.entries(node).flatMap(([k, v]) =>
      flatten(v as Node, prefix ? `${prefix}.${k}` : k),
    );
  }
  return [];
}

describe("i18n 文案可编译性", () => {
  for (const [locale, messages] of [
    ["zh", zh],
    ["en", en],
  ] as const) {
    it(`${locale} 全部文案都能通过消息编译器`, () => {
      const failures: string[] = [];
      for (const [path, text] of flatten(messages as Node)) {
        // baseCompile 不抛异常，编译错误只经 onError 回调上报；
        // 用 try/catch 包一层会静默通过，等于没测。
        const errors: string[] = [];
        baseCompile(text, { onError: (e) => errors.push(e.message) });
        if (errors.length > 0) {
          failures.push(
            `${path}\n    文案: ${text}\n    错误: ${errors.join(" / ")}`,
          );
        }
      }
      expect(
        failures,
        `以下文案编译失败，渲染到时会白屏。裸露的 @ 需写成 {'@'}：\n  ` +
          failures.join("\n  "),
      ).toHaveLength(0);
    });
  }
});
