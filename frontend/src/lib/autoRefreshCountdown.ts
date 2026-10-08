// 自动刷新倒计时的一步：给定截止时间，返回当前显示的剩余秒数，以及到下一次
// 秒数变化（或到达截止时间）还要等多久。按截止时间计算而不是累减，
// 定时器延迟或页面被节流时显示仍然准确。
export function autoRefreshCountdownStep(
  deadline: number,
  now: number,
): { remaining: number; delay: number } {
  const left = deadline - now;
  if (left <= 0) return { remaining: 0, delay: 0 };
  const remaining = Math.ceil(left / 1000);
  return { remaining, delay: left - (remaining - 1) * 1000 };
}
