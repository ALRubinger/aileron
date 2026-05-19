(function () {
  var target = document.currentScript;
  window.addEventListener('load', function () {
    calendar.schedulingButton.load({
      url: 'https://calendar.google.com/calendar/appointments/schedules/AcZssZ0due3tfhNZobPoGzpCoUnKo3rLe5C0K_BnAWbLf41Kv7ZoK4UeweVCll-eRHBNu9GWzjT3d8re?gv=true',
      color: '#039BE5',
      label: 'Book an appointment',
      target: target,
    });
  });
})();
