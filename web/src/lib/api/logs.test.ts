import { describe, it, expect } from 'vitest'
import { csvRecordCount } from './logs'
describe('日志导出完整性', () => {
  it('按 CSV 记录计数而不是按正文换行计数', () => {
    expect(csvRecordCount('id,message\n1,"line1\nline2, ""quote"""\n')).toBe(2)
    expect(csvRecordCount('id,message\n1,last')).toBe(2)
    expect(() => csvRecordCount('id,message\n1,"unterminated')).toThrow()
  })
})
