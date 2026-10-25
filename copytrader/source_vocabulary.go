package copytrader

import "regexp"

// Source status vocabulary: the single home of the exit / retained-position /
// recap wording shared by the deterministic interpreters, the preset policy and
// AI evidence validation. Patterns that look alike are NOT interchangeable; the
// comments state how each differs. Changing any of them changes how existing
// channels (Tyler, Jonzi, Neil, DSC) are interpreted, so add regression cases.

// --- Exit / closed wording -------------------------------------------------
//
// sourceCloseIntent is the broad "close it" INTENT, including imperative verbs
// (全平, close, exit, cut it, 提前…離場) and closed-status phrases. Used to
// accept a full close.
//
// sourceClosedStatus only matches REPORTED status ("trade was closed", 已平倉,
// 已經出場, stopped out). It distinguishes a status recap from an imperative
// close and locates status phrases inside a message (FindAllStringIndex).
//
// sourceFullProfitExit is "止盈全部倉位": a full exit phrased as take-profit,
// valid only when no retained-position wording is present.
//
// jonziTerminal is the jonzi trade-card terminal status line ("stop/loss was
// hit", "trailed stop was hit", 交易已手動平倉), narrower and card-specific.
var sourceCloseIntent = regexp.MustCompile(`(?i)(全平|平[倉仓]|提前.*(?:止[損损](?:出局|出場|出|離場|离场|退出)|[離离][場场]|退出|出場|出场)|止[損损].*(?:已觸發|已触发|出局|出場|离场|離場|退出)|取消[倉仓]位|\bcancel.*\bposition\b|\b(?:close|closing|exit|out|cut it)(?:\s+here)?\b|manually closed|stopped out|stop(?:ped)? .*hit|trade (?:was )?closed|position closed|已平[倉仓]|交易已平[倉仓])`)
var sourceClosedStatus = regexp.MustCompile(`(?i)(交易已平[倉仓]|已平[倉仓]|trade (?:was )?closed|position closed|manually closed|stopped out|stop(?:ped)? .*hit|止[損损].*(?:已觸發|已触发|出局|退出)|已經出場|已经出场)`)
var sourceFullProfitExit = regexp.MustCompile(`止盈\s*(?:全部|所有)\s*[倉仓]位`)
var jonziTerminal = regexp.MustCompile(`(?i)(trade was (?:manually )?closed|stop/loss was hit|trailed stop was hit|交易已(?:手动|手動)?(?:平仓|平倉|关闭|關閉)|(?:跟踪|追踪|跟蹤|追蹤)?止[損损].*(?:被觸發|被触发|已觸發|已触发))`)

// bareCloseWording: the English close verbs (without the trailing "here" form)
// plus 這單就不拿, accepted as close evidence for an AI CLOSE action.
var bareCloseWording = regexp.MustCompile(`(?i)(\b(?:close|closing|exit|out|cut it)\b|[這这][單单]就不拿)`)

// earlyTPWording / partialCloseRatio mark a TYLER reduce instruction
// ("提前 TP1", "平倉 30%").
var earlyTPWording = regexp.MustCompile(`(?i)(提前|可做|可以作|自行.*作)\s*TP\s*\d`)
var partialCloseRatio = regexp.MustCompile(`(?:平倉|平仓)\s*[0-9]+(?:\.[0-9]+)?\s*[%％]`)

// --- Retained position -------------------------------------------------------
//
// retainedPosition is a boolean gate ("留 30%", "剩三成" with 成) that blocks a
// full exit; retainedV4 is the TYLER v4 extractor: it captures the percentage
// and accepts only %/％ (not 成). Kept separate because v4 rules are frozen.
var retainedPosition = regexp.MustCompile(`(?:留|保留|剩)[下著着餘余]?\s*[0-9]+(?:\.[0-9]+)?\s*[%％成]`)
var retainedV4 = regexp.MustCompile(`(?:留|保留|剩)[下著着餘余]?\s*([0-9]+(?:\.[0-9]+)?)\s*[%％]`)

// --- Recap / profit report -----------------------------------------------------
//
// recapOnly is the preset-policy gate for a pure profit recap (收益, 起飛, "TP1
// hit") that must not trigger a reduce/close.
// sourceProfit is the source-segmentation profit report (adds 翻倍/倍, "booked"
// and the bare "#SYM TP1" form): it marks reference sources and recap-only
// bodies while splitting a message into sources.
// sourceTPRecap is stripped from the text before checking for current
// management wording: reporting that a TP was hit is not itself a new
// management instruction.
var recapOnly = regexp.MustCompile(`(?i)(收益|获利|獲利|起飞|起飛|(?:TP\s*\d|止盈).*(?:到达|到達|已到|hit|完成|✅))`)
var sourceProfit = regexp.MustCompile(`(?i)(起飛|起飞|獲利|获利|翻倍|倍|TP\s*\d.*(?:到|完成|✅|hit|booked)|^\s*[#＃]\S+\s+TP\s*\d\s*(?:<.*>)?\s*$)`)
var sourceTPRecap = regexp.MustCompile(`(?i)止盈\s*(?:已)?(?:到達|到达|到了|已到|到|完成|hit|✅)`)
