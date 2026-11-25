'use strict';
// 共享几何图标。字体字形的墨迹中心随平台字体度量漂移（SF/PingFang 上
// “×”普遍比按钮几何中心低 1~2px 且墨迹偏小），SVG 图标不受字体影响，
// 由按钮的网格居中精确对齐。viewBox 统一 24，默认渲染 18px、1.8 描边，
// 与页头网关图标（20px/1.6）的落墨粗细一致。
const BridgeIcons={
  icon(path,width=18,strokeWidth=1.8){
    const svg=document.createElementNS('http://www.w3.org/2000/svg','svg');
    svg.setAttribute('viewBox','0 0 24 24');
    svg.setAttribute('width',String(width));svg.setAttribute('height',String(width));
    svg.setAttribute('fill','none');svg.setAttribute('stroke','currentColor');
    svg.setAttribute('stroke-width',String(strokeWidth));svg.setAttribute('stroke-linecap','round');
    svg.setAttribute('aria-hidden','true');
    const line=document.createElementNS('http://www.w3.org/2000/svg','path');
    line.setAttribute('d',path);svg.append(line);
    return svg;
  },
  close(){return this.icon('M6 6l12 12M18 6 6 18');},
  plus(){return this.icon('M12 5v14M5 12h14');}
};
if(typeof module!=='undefined')module.exports=BridgeIcons;
