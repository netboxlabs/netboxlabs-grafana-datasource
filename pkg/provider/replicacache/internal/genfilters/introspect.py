# Read-only introspection of NetBox's REST API filtersets, for genfilters.
#
# For every model whose API viewset has a filterset_class it lists each entry
# of the filterset's base_filters (lookup variants such as site__n are their own
# entries) with what genfilters needs to say what NetBox means by it: the filter
# class, the field it walks, the lookup, the model columns that walk reaches, and
# what the literal "null" does. It runs inside a NetBox container, once per
# NetBox version, and writes JSON to stdout:
#
#	docker exec -i <netbox 4.4.10 container> /opt/netbox/venv/bin/python - \
#	  < introspect.py > fs-4.4.10.json
#	docker exec -i <netbox 4.6.10 container> /opt/netbox/venv/bin/python - \
#	  < introspect.py > fs-4.6.10.json
#
# It reads and never writes: compiling a query (str() of a queryset's query) runs
# nothing, and building a filterset only reads NetBox's custom-field definitions.
import importlib
import json
import os
import sys
import warnings

# NetBox's container configuration prints a line for every file it loads. The
# JSON is the only thing that may reach stdout, so everything until the final
# dump goes to stderr.
json_out, sys.stdout = sys.stdout, sys.stderr

sys.path.insert(0, '/opt/netbox/netbox')
os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'netbox.settings')
warnings.filterwarnings('ignore')

import django  # noqa: E402

django.setup()

from django.conf import settings  # noqa: E402
from django.core.exceptions import FieldDoesNotExist  # noqa: E402
from django.http import QueryDict  # noqa: E402

try:
    from mptt.models import MPTTModel
except ImportError:  # a NetBox without django-mptt has no MPTT targets
    MPTTModel = None
try:
    from django.contrib.contenttypes.fields import GenericForeignKey, GenericRel, GenericRelation
    GENERIC = (GenericForeignKey, GenericRelation, GenericRel)
except ImportError:
    GENERIC = ()

APPS = ['circuits', 'core', 'dcim', 'extras', 'ipam', 'tenancy', 'users', 'virtualization', 'vpn', 'wireless']

# The classes and lookups where the literal "null" can mean IS NULL, so that the
# probe below is worth running: django-filter's MultipleChoiceFilter turns the
# filter's null_value into None, and Django reads exact=None and iexact=None as
# IS NULL. Any other lookup raises on None, and the number filters' forms refuse
# the text.
PROBE_CLASSES = {'MultiValueCharFilter', 'MultiValueMACAddressFilter', 'ChoiceFilter', 'MultipleChoiceFilter'}
PROBE_LOOKUPS = {'exact', 'iexact'}


def is_tree(model):
    return MPTTModel is not None and issubclass(model, MPTTModel)


def hop_kind(field):
    if GENERIC and isinstance(field, GENERIC):
        return 'generic'
    if not getattr(field, 'is_relation', False):
        return 'local'
    if getattr(field, 'auto_created', False) and not getattr(field, 'concrete', False):
        return 'reverse'
    if getattr(field, 'many_to_many', False):
        return 'm2m'
    if getattr(field, 'one_to_one', False):
        return 'o2o'
    if getattr(field, 'many_to_one', False):
        return 'fk'
    return 'other:' + type(field).__name__


def walk(model, field_name):
    """Follow field_name.split("__") from model, one hop per part, and stop after
    the first part that is not a relation. A part that is not a field of the
    model at all (a method filter's own name) is a hop of kind "missing".

    column is the database column of a local field or a foreign key, as Django
    names it: site_id -> site_id, _site -> _site_id, _children -> _children. It is
    never built from the part, which is why nothing downstream appends _id. A
    many-to-many, a reverse or a generic relation has no column in this table,
    whatever Django reports for it (a taggable manager reports "tags")."""
    hops = []
    if not field_name:
        return hops
    current = model
    for part in field_name.split('__'):
        try:
            field = current._meta.get_field(part)
        except FieldDoesNotExist:
            hops.append({'part': part, 'kind': 'missing'})
            break
        kind = hop_kind(field)
        hop = {'part': part, 'kind': kind}
        if kind in ('local', 'fk', 'o2o'):
            hop['column'] = getattr(field, 'column', None)
        related = getattr(field, 'related_model', None)
        if related is not None and not isinstance(related, str):
            hop['target'] = related._meta.label_lower
            hop['target_tree'] = is_tree(related)
        hops.append(hop)
        if kind == 'local' or related is None or isinstance(related, str):
            break
        current = related
    return hops


def method_name(method):
    if method is not None and not isinstance(method, str):
        return getattr(method, '__name__', str(method))
    return method


def null_label(filter_):
    """The form field's null_label: None when the field has no null choice
    (cable-terminations' cable) and 'None' on every declared related filter."""
    return getattr(filter_.field, 'null_label', None)


def null_probe(filterset_class, model, name):
    """Whether <name>=null is a valid filter that compiles to SQL with IS NULL.
    The query is compiled and never run. Sites' time_zone compares null as '' and
    compiles to no IS NULL (nor does its negation: that one holds IS NOT NULL)."""
    filterset = filterset_class(QueryDict(name + '=null'), queryset=model.objects.all())
    if not filterset.is_valid():
        return False
    return 'IS NULL' in str(filterset.qs.query)


def describe(filterset_class, model, name, filter_):
    # An exception here is a NetBox this walk does not understand. It stops the
    # run and names the filter, instead of being read as "no null choice" or "no
    # null" and written into a vocabulary that is wrong without saying so.
    try:
        return describe_filter(filterset_class, model, name, filter_)
    except Exception as exc:
        raise RuntimeError('%s %s: %s: %s' % (model._meta.label_lower, name, type(exc).__name__, exc)) from exc


def describe_filter(filterset_class, model, name, filter_):
    method = method_name(getattr(filter_, 'method', None))
    null_value = getattr(filter_, 'null_value', None)
    entry = {
        'name': name,
        'cls': type(filter_).__name__,
        'field_name': filter_.field_name,
        'lookup': filter_.lookup_expr,
        'exclude': bool(filter_.exclude),
        'method': method,
        'to_field': filter_.extra.get('to_field_name'),
        'null_label': null_label(filter_),
        'null_value': null_value if isinstance(null_value, str) else None,
        'hops': walk(model, filter_.field_name),
    }
    if (method is None and entry['cls'] in PROBE_CLASSES and entry['lookup'] in PROBE_LOOKUPS
            and entry['null_value'] == 'null'):
        entry['null_probe'] = null_probe(filterset_class, model, name)
    return entry


models = {}
for app in APPS:
    try:
        urls = importlib.import_module(app + '.api.urls')
    except ImportError:
        continue
    router = getattr(urls, 'router', None)
    if router is None:
        continue
    for prefix, viewset, basename in router.registry:
        queryset = getattr(viewset, 'queryset', None)
        filterset_class = getattr(viewset, 'filterset_class', None)
        # A viewset that builds its queryset in get_queryset has none to read, and
        # core/object-changes on NetBox 4.4 is one; its filterset still says which
        # model it filters.
        if queryset is not None:
            model = queryset.model
        else:
            model = getattr(getattr(filterset_class, '_meta', None), 'model', None)
        if filterset_class is None or model is None:
            print('skipped %s/%s: no %s' % (app, prefix, 'filterset_class' if filterset_class is None else 'model'), file=sys.stderr)
            continue
        label = model._meta.label_lower
        if label in models:
            # core/object-types is also routed as extras/object-types, for old
            # clients. The same filterset makes it one model; two different ones
            # would be two vocabularies for one table, which this cannot merge.
            if models[label]['filterset'] != filterset_class.__name__:
                raise SystemExit('%s has two API viewsets with different filtersets: %s (%s) and %s/%s (%s)' % (
                    label, models[label]['path'], models[label]['filterset'], app, prefix, filterset_class.__name__))
            print('skipped %s/%s: %s is already listed as %s' % (app, prefix, label, models[label]['path']), file=sys.stderr)
            continue
        models[label] = {
            'path': '%s/%s' % (app, prefix),
            'filterset': filterset_class.__name__,
            'filters': [describe(filterset_class, model, name, f) for name, f in filterset_class.base_filters.items()],
        }

sys.stdout = json_out
json.dump({'version': getattr(settings, 'VERSION', None), 'models': dict(sorted(models.items()))}, sys.stdout, separators=(',', ':'))
