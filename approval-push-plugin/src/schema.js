/**
 * Schema 兼容层。
 *
 * DSH 运行时提供 `@deepseek-ai/schemastery`；但插件单测（以及 CI 里没有
 * DSH profile 的环境）解析不到这个包。这里优先用真包，缺失时退回一个
 * 只覆盖本插件用到的 API 的最小实现——保证同一份代码在两种环境都能加载。
 */
let Schema
try {
  Schema = (await import('@deepseek-ai/schemastery')).default
} catch {
  class Field {
    constructor(type) { this.type = type; this.requiredFlag = false; this.defaultValue = undefined }
    required() { this.requiredFlag = true; return this }
    default(v) { this.defaultValue = v; return this }
    min(v) { this.minValue = v; return this }
    max(v) { this.maxValue = v; return this }
  }
  const check = (name, field, value) => {
    if (field.type === 'number') {
      if (typeof value !== 'number' || Number.isNaN(value)) throw new Error(`${name} must be a number`)
      if (field.minValue !== undefined && value < field.minValue) throw new Error(`${name} below min`)
      if (field.maxValue !== undefined && value > field.maxValue) throw new Error(`${name} above max`)
    }
    if (field.type === 'boolean' && typeof value !== 'boolean') throw new Error(`${name} must be a boolean`)
    if (field.type === 'string' && typeof value !== 'string') throw new Error(`${name} must be a string`)
    return value
  }
  Schema = {
    string: () => new Field('string'),
    boolean: () => new Field('boolean'),
    number: () => new Field('number'),
    object: (shape) => (input = {}) => {
      const out = {}
      for (const [name, field] of Object.entries(shape)) {
        let value = input[name]
        if (value === undefined) value = field.defaultValue
        if (value === undefined) {
          if (field.requiredFlag) throw new Error(`missing required config: ${name}`)
          continue
        }
        out[name] = check(name, field, value)
      }
      return out
    },
  }
}
export default Schema
