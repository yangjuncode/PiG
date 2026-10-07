/* PiG 学习工作区 · 可复用测验组件
   用法：
     <div class="quiz" data-quiz>
       <div class="quiz-q" data-answer="b">
         <p class="quiz-stem">题干</p>
         <ul class="quiz-opts">
           <li><button data-opt="a">选项 A</button></li>
           <li><button data-opt="b">选项 B</button></li>
         </ul>
         <p class="quiz-explain" hidden>解析（答完显示）</p>
       </div>
     </div>
   规则：每题只能答一次；答完显示解析；全部答完显示总分。 */
(function () {
  "use strict";

  function initQuiz(root) {
    var questions = Array.prototype.slice.call(root.querySelectorAll(".quiz-q"));
    var answered = 0;
    var correct = 0;

    var score = document.createElement("p");
    score.className = "quiz-score";
    score.hidden = true;
    root.appendChild(score);

    questions.forEach(function (q, idx) {
      var answer = q.getAttribute("data-answer");
      var explain = q.querySelector(".quiz-explain");
      var buttons = Array.prototype.slice.call(q.querySelectorAll("button[data-opt]"));

      buttons.forEach(function (btn) {
        btn.addEventListener("click", function () {
          if (q.dataset.done) return;
          q.dataset.done = "1";
          answered += 1;

          var picked = btn.getAttribute("data-opt");
          buttons.forEach(function (b) {
            b.disabled = true;
            if (b.getAttribute("data-opt") === answer) b.classList.add("correct");
          });
          if (picked === answer) {
            correct += 1;
          } else {
            btn.classList.add("wrong");
          }
          if (explain) explain.hidden = false;

          if (answered === questions.length) {
            score.hidden = false;
            score.textContent = "得分：" + correct + " / " + questions.length +
              (correct === questions.length ? " —— 全部正确，可以进入下一课" : " —— 建议回看错题对应的小节");
          }
        });
      });

      // 给题号一个稳定的锚点 id，便于课程内互相引用
      if (!q.id) q.id = "q" + (idx + 1);
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll("[data-quiz]").forEach(initQuiz);
  });
})();
