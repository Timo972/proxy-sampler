import { describe, expect, it } from 'vitest'
import { readFileSync } from 'node:fs'

const styles = readFileSync(`${process.cwd()}/src/index.css`, 'utf8')

describe('responsive CSS contract', () => {
  it('prevents overflow structurally instead of clipping the page', () => {
    expect(styles).not.toMatch(/(?:html|body)\s*\{[^}]*overflow-x:\s*(?:hidden|clip)/)
    expect(styles).not.toMatch(/\.no-page-overflow\s*\{/)
    expect(styles).toMatch(/\.topbar-inner,\s*\.app-content\s*\{[^}]*min-width:\s*0/)
    expect(styles).toMatch(/\.home-page\s*\{[^}]*min-width:\s*0[^}]*max-width:\s*100%/)
    expect(styles).toMatch(/\.session-sections\s*\{[^}]*min-width:\s*0[^}]*max-width:\s*100%/)
  })

  it('gives the mobile product home link a 44px hit target', () => {
    const mobileStyles = styles.slice(styles.indexOf('@media (max-width: 759px)'), styles.indexOf('@media (prefers-reduced-motion: reduce)'))
    expect(mobileStyles).toMatch(/\.product-name\s*\{[^}]*min-height:\s*2\.75rem/)
  })

  it('wraps long mobile session names and values', () => {
    const mobileStyles = styles.slice(styles.indexOf('@media (max-width: 759px)'), styles.indexOf('@media (prefers-reduced-motion: reduce)'))
    expect(mobileStyles).toMatch(/\.record-title\s*\{[^}]*min-width:\s*0[^}]*overflow-wrap:\s*anywhere/)
    expect(mobileStyles).toMatch(/\.session-record dd\s*\{[^}]*max-width:\s*62%[^}]*overflow-wrap:\s*anywhere/)
  })

  it('keeps the report structural, scroll-contained, and touch operable without fluid type or viewport bleed', () => {
  const mobileStyles = styles.slice(styles.lastIndexOf('@media (max-width: 759px)'))
  expect(styles).toMatch(/\.session-page\s*\{[^}]*min-width:\s*0[^}]*max-width:\s*100%/)
  expect(styles).toMatch(/\.sample-table-wrap\s*\{[^}]*max-width:\s*100%[^}]*overflow-x:\s*auto/)
  expect(styles).not.toMatch(/\.session-title h1\s*\{[^}]*font-size:\s*clamp\(/)
  expect(mobileStyles).not.toMatch(/\.sample-table-wrap\s*\{[^}]*margin-inline-end:\s*calc\(/)
  expect(mobileStyles).toMatch(/\.chart-data summary,\s*\.sample-disclosure,\s*\.clipped-value\s*\{[^}]*min-height:\s*2\.75rem/)
  expect(mobileStyles).toMatch(/\.ip-details-trigger\s*\{[^}]*min-height:\s*2\.75rem/)
  })
})
