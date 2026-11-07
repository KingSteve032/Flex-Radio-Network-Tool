'use strict';
'require view';
'require form';
'require fs';
'require ui';

// Runs only explicitly whitelisted service commands through rpcd ACL.
// Never grant rpcd access to /bin/sh or a general command interpreter.
var QUERY = '/usr/libexec/frnt-client-query';
var CONTROL = '/usr/libexec/frnt-client-control';

function runRead(action) {
	return fs.exec(QUERY, [action]).then(function(result) {
		return result.stdout || result.stderr || '(no output)';
	});
}

return view.extend({
	load: function() {
		return Promise.all([
			L.resolveDefault(runRead('status'), 'Status unavailable'),
			L.resolveDefault(runRead('vpn'), 'NetBird interface unavailable'),
			L.resolveDefault(runRead('logs'), 'No FRNT log entries')
		]);
	},

	render: function(data) {
		var m, s, o;
		m = new form.Map('frnt-client', _('FRNT Discovery Client'),
			_('Receive remote FlexRadio discovery over NetBird and rebroadcast on your router LAN (UDP 4992).'));
		s = m.section(form.NamedSection, 'main', 'frnt-client', _('Connection settings'));
		s.anonymous = true;

		o = s.option(form.Value, 'server', _('FRNT server VPN IPv4'),
			_('The server NetBird IPv4 address. Leave empty only if automatic NetBird route discovery is enabled.'));
		o.placeholder = '100.85.50.97';
		o.datatype = 'or(ip4addr,empty)';

		o = s.option(form.Flag, 'netbird_routes', _('Discover server using NetBird routes'),
			_('Requires "netbird routes list" to advertise exactly one selected Flextool /32 host route. Manual IP is more reliable on older NetBird builds.'));
		o.default = '0';
		o.rmempty = false;

		o = s.option(form.Value, 'vpn_interface', _('NetBird interface'));
		o.placeholder = 'wt0';
		o.default = 'wt0';
		o.rmempty = false;

		o = s.option(form.Value, 'lan_interface', _('LAN broadcast interface'));
		o.placeholder = 'br-lan';
		o.default = 'br-lan';
		o.rmempty = false;

		var statusText = E('strong', {}, data[0]);
		var vpnText = E('pre', { 'style': 'white-space:pre-wrap;overflow-x:auto;' }, data[1]);
		var logText = E('pre', {
			'style': 'white-space:pre-wrap;overflow-x:auto;max-height:20em;overflow-y:auto;'
		}, data[2]);

		function refresh() {
			return Promise.all([
				L.resolveDefault(runRead('status'), 'Status unavailable'),
				L.resolveDefault(runRead('vpn'), 'NetBird interface unavailable'),
				L.resolveDefault(runRead('logs'), 'No FRNT log entries')
			]).then(function(values) {
				statusText.textContent = values[0];
				vpnText.textContent = values[1];
				logText.textContent = values[2];
			});
		}

		function actionButton(title, verb, style) {
			return E('button', {
				'class': 'btn cbi-button ' + style,
				'type': 'button',
				'click': function(ev) {
					ev.preventDefault();
					var button = ev.currentTarget;
					button.disabled = true;
					return fs.exec(CONTROL, [verb]).then(function(result) {
						if (result.code !== 0) {
							ui.addNotification(null, E('p', {},
								_('Service operation failed: ') + (result.stderr || result.stdout || result.code)), 'danger');
						}
						return refresh();
					}).catch(function(err) {
						ui.addNotification(null, E('p', {}, String(err)), 'danger');
					}).finally(function() {
						button.disabled = false;
					});
				}
			}, title);
		}

		var controls = E('div', { 'class': 'cbi-section' }, [
			E('h3', {}, _('Service status')),
			E('p', {}, [ _('FRNT: '), statusText ]),
			E('div', { 'style': 'display:flex;flex-wrap:wrap;gap:0.6em;' }, [
				actionButton(_('Start'), 'start', 'cbi-button-apply'),
				actionButton(_('Stop'), 'stop', 'cbi-button-reset'),
				actionButton(_('Restart'), 'restart', 'cbi-button-action'),
				E('button', {
					'class': 'btn cbi-button',
					'type': 'button',
					'click': function(ev) {
						ev.preventDefault();
						return refresh();
					}
				}, _('Refresh'))
			]),
			E('p', { 'class': 'description' },
				_('Save & Apply settings before using Restart. Start/Stop controls only the FRNT client, never NetBird.')),
			E('h4', {}, _('NetBird VPN address')), vpnText,
			E('h4', {}, _('Recent FRNT logs')), logText,
			E('p', { 'class': 'description' },
				_('UDP connection refused means the remote FRNT listener or firewall is rejecting port 14992. Check the server SEND_INTERFACE and listening socket.'))
		]);

		return m.render().then(function(config) {
			return E('div', {}, [controls, config]);
		});
	}
});
