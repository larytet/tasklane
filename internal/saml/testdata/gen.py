#!/usr/bin/env python3
"""Generates the SAML test fixtures with lxml and cryptography, which share no code with the Go verifier: lxml's exclusive canonicalization
(libxml2) is the reference. Run `python3 gen.py` here to regenerate; the outputs are committed, and the keys are for tests only.

Fixed values the tests use: request id _req1, ACS https://tasklane.example/api/v1/auth/saml/acs, SP entity
https://tasklane.example/api/v1/auth/saml/metadata, IdP entity https://idp.example/entity, assertion valid 09:55 to 10:05 on 2026-03-01."""
import base64, datetime, hashlib, os, copy
from lxml import etree
from cryptography import x509
from cryptography.x509.oid import NameOID
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa, padding

HERE = os.path.dirname(os.path.abspath(__file__))
NS_P = "urn:oasis:names:tc:SAML:2.0:protocol"
NS_A = "urn:oasis:names:tc:SAML:2.0:assertion"
NS_DS = "http://www.w3.org/2000/09/xmldsig#"
NS_XS = "http://www.w3.org/2001/XMLSchema"
NS_XSI = "http://www.w3.org/2001/XMLSchema-instance"
ACS = "https://tasklane.example/api/v1/auth/saml/acs"
SP = "https://tasklane.example/api/v1/auth/saml/metadata"
IDP = "https://idp.example/entity"

def mkkey(name, cn):
    path = os.path.join(HERE, name)
    if os.path.exists(path + ".key"):
        key = serialization.load_pem_private_key(open(path + ".key", "rb").read(), None)
        cert = x509.load_pem_x509_certificate(open(path + ".pem", "rb").read())
        return key, cert
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, cn)])
    cert = (x509.CertificateBuilder().subject_name(subject).issuer_name(subject).public_key(key.public_key())
            .serial_number(x509.random_serial_number()).not_valid_before(datetime.datetime(2025, 1, 1))
            .not_valid_after(datetime.datetime(2046, 1, 1)).sign(key, hashes.SHA256()))
    open(path + ".key", "wb").write(key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()))
    open(path + ".pem", "wb").write(cert.public_bytes(serialization.Encoding.PEM))
    return key, cert

IDPKEY, IDPCERT = mkkey("idp", "idp.example")
OTHERKEY, _ = mkkey("other", "other.example")

HASH = {"sha256": (hashes.SHA256, hashlib.sha256, "http://www.w3.org/2001/04/xmlenc#sha256", "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"),
        "sha512": (hashes.SHA512, hashlib.sha512, "http://www.w3.org/2001/04/xmlenc#sha512", "http://www.w3.org/2001/04/xmldsig-more#rsa-sha512"),
        "sha1": (hashes.SHA1, hashlib.sha1, "http://www.w3.org/2000/09/xmldsig#sha1", "http://www.w3.org/2000/09/xmldsig#rsa-sha1")}

def c14n(el, inclusive=None):
    return etree.tostring(el, method="c14n", exclusive=True, with_comments=False, inclusive_ns_prefixes=inclusive)

def sub(parent, ns, tag, text=None, **attrs):
    e = etree.SubElement(parent, "{%s}%s" % (ns, tag), **attrs)
    if text is not None:
        e.text = text
    return e

def reparse(el):
    """What a verifier sees is the serialized document, not our in-memory tree (lxml canonicalizes the two differently when inclusive prefixes are
    involved), so every signature is made over the tree as it reads back from its serialization."""
    return etree.fromstring(etree.tostring(el))

def sign(el, key=None, alg="sha256", inclusive=None, ds_prefix="ds", transforms=True):
    key = key or IDPKEY
    hcls, hlib, dig_uri, sig_uri = HASH[alg]
    digest = base64.b64encode(hlib(c14n(el, inclusive)).digest()).decode()
    nsmap = {ds_prefix: NS_DS} if ds_prefix else {None: NS_DS}
    sig = etree.Element("{%s}Signature" % NS_DS, nsmap=nsmap)
    si = sub(sig, NS_DS, "SignedInfo")
    sub(si, NS_DS, "CanonicalizationMethod", Algorithm="http://www.w3.org/2001/10/xml-exc-c14n#")
    sub(si, NS_DS, "SignatureMethod", Algorithm=sig_uri)
    ref = sub(si, NS_DS, "Reference", URI="#" + el.get("ID"))
    if transforms:
        tr = sub(ref, NS_DS, "Transforms")
        sub(tr, NS_DS, "Transform", Algorithm="http://www.w3.org/2000/09/xmldsig#enveloped-signature")
        t2 = sub(tr, NS_DS, "Transform", Algorithm="http://www.w3.org/2001/10/xml-exc-c14n#")
        if inclusive:
            sub(t2, "http://www.w3.org/2001/10/xml-exc-c14n#", "InclusiveNamespaces", PrefixList=" ".join(inclusive))
    sub(ref, NS_DS, "DigestMethod", Algorithm=dig_uri)
    sub(ref, NS_DS, "DigestValue", digest)
    sv = sub(sig, NS_DS, "SignatureValue")
    ki = sub(sig, NS_DS, "KeyInfo")
    x5 = sub(ki, NS_DS, "X509Data")
    sub(x5, NS_DS, "X509Certificate", base64.b64encode(IDPCERT.public_bytes(serialization.Encoding.DER)).decode())
    # the signature goes after the Issuer, as the schema says; the digest above was taken without it
    issuer = el.find("{%s}Issuer" % NS_A)
    issuer.addnext(sig)
    sv.text = base64.b64encode(key.sign(c14n(si), padding.PKCS1v15(), hcls())).decode()
    return sig

def response(email="ana@example.com", name="Ana Novak", rid="_resp1", aid="_assert1", req="_req1", dest=ACS, recipient=ACS, audience=SP,
             idp=IDP, nb="2026-03-01T09:55:00Z", na="2026-03-01T10:05:00Z", status="urn:oasis:names:tc:SAML:2.0:status:Success", conditions=True,
             nsmap=None, attrs=True, special=False, default_ns=False):
    nsmap = nsmap or {"samlp": NS_P, "saml": NS_A, "xs": NS_XS, "xsi": NS_XSI}
    r = etree.Element("{%s}Response" % NS_P, nsmap=nsmap, ID=rid, Version="2.0", IssueInstant="2026-03-01T10:00:00Z", Destination=dest, InResponseTo=req)
    sub(r, NS_A, "Issuer", idp)
    st = sub(r, NS_P, "Status")
    sub(st, NS_P, "StatusCode", Value=status)
    a = sub(r, NS_A, "Assertion", ID=aid, Version="2.0", IssueInstant="2026-03-01T10:00:00Z")
    sub(a, NS_A, "Issuer", idp)
    s = sub(a, NS_A, "Subject")
    sub(s, NS_A, "NameID", email, Format="urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress")
    sc = sub(s, NS_A, "SubjectConfirmation", Method="urn:oasis:names:tc:SAML:2.0:cm:bearer")
    sub(sc, NS_A, "SubjectConfirmationData", NotOnOrAfter=na, Recipient=recipient, InResponseTo=req)
    if conditions:
        c = sub(a, NS_A, "Conditions", NotBefore=nb, NotOnOrAfter=na)
        ar = sub(c, NS_A, "AudienceRestriction")
        sub(ar, NS_A, "Audience", audience)
    ast = sub(a, NS_A, "AuthnStatement", AuthnInstant="2026-03-01T10:00:00Z", SessionIndex="_sess1")
    ctx = sub(ast, NS_A, "AuthnContext")
    sub(ctx, NS_A, "AuthnContextClassRef", "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport")
    if attrs:
        stt = sub(a, NS_A, "AttributeStatement")
        for n, v in (("email", email), ("displayName", name if not special else 'Ana <"Nova&k"> Åž\nsecond line')):
            at = sub(stt, NS_A, "Attribute", Name=n)
            val = sub(at, NS_A, "AttributeValue", v)
            val.set("{%s}type" % NS_XSI, "xs:string")
    return r

def signed(r, target="assertion", **kw):
    """Reparse, sign the assertion (or the response), and return the tree."""
    r = reparse(r)
    el = assertion(r) if target == "assertion" else r
    sign(el, **kw)
    return r

def write(name, el):
    data = etree.tostring(el, xml_declaration=False, encoding="UTF-8")
    open(os.path.join(HERE, name + ".xml"), "wb").write(data)

def assertion(r):
    return r.find("{%s}Assertion" % NS_A)

# ---- the good ones ----
r = signed(response()); write("assertion_signed", r)
r = signed(response(), target="response"); write("response_signed", r)
r = reparse(response()); sign(assertion(r)); r = reparse(r); sign(r); write("both_signed", r)
r = signed(response(), inclusive=["xs", "xsi"]); write("inclusive_prefix", r)
r = signed(response(special=True)); write("special_chars", r)
r = signed(response(), alg="sha512"); write("sha512", r)
r = signed(response(nsmap={"samlp": NS_P, "saml": NS_A, "xs": NS_XS, "xsi": NS_XSI, "unused": "urn:unused"}), ds_prefix=None); write("default_ds_ns", r)
# prefixes and declarations the way other implementations write them: different prefixes, declarations on the assertion itself
r = signed(response(nsmap={"p": NS_P, "a": NS_A}), ds_prefix="dsig"); write("other_prefixes", r)

# ---- the refused ones ----
r = signed(response(), key=OTHERKEY); write("other_key", r)
r = response(); write("unsigned", r)
r = signed(response(), alg="sha1"); write("sha1", r)
r = signed(response(audience="https://someone.else/entity")); write("wrong_audience", r)
r = signed(response(na="2026-03-01T09:00:00Z", nb="2026-03-01T08:00:00Z")); write("expired", r)
r = signed(response(nb="2026-03-01T11:00:00Z", na="2026-03-01T12:00:00Z")); write("not_yet", r)
r = signed(response(recipient="https://evil.example/acs")); write("wrong_recipient", r)
r = signed(response(req="_other")); write("wrong_inresponse", r)
r = signed(response(dest="https://evil.example/acs")); write("wrong_destination", r)
r = signed(response(idp="https://evil-idp.example/entity")); write("wrong_issuer", r)
r = signed(response(conditions=False)); write("no_conditions", r)
r = signed(response(status="urn:oasis:names:tc:SAML:2.0:status:AuthnFailed")); write("status_failure", r)

# signature wrapping: an unsigned assertion about someone else beside (or in place of) the signed one
def evil_assertion(r):
    e = copy.deepcopy(assertion(r))
    for sig in e.findall("{%s}Signature" % NS_DS):
        e.remove(sig)
    e.find("{%s}Subject/{%s}NameID" % (NS_A, NS_A)).text = "admin@example.com"
    for v in e.iter("{%s}AttributeValue" % NS_A):
        if v.text == "ana@example.com":
            v.text = "admin@example.com"
    return e
r = reparse(response()); sign(assertion(r)); good = assertion(r)
ev = evil_assertion(r); ev.set("ID", "_evil"); good.addprevious(ev); write("xsw_two_assertions", r)
r = reparse(response()); sign(assertion(r)); good = assertion(r)
ev = evil_assertion(r); good.addprevious(ev)  # the same ID as the signed one
write("xsw_duplicate_id", r)
r = reparse(response()); sign(assertion(r)); good = assertion(r)
ext = etree.Element("{%s}Extensions" % NS_P); r.find("{%s}Status" % NS_P).addnext(ext)
ext.append(copy.deepcopy(good)); r.remove(good)
ev = evil_assertion(r.find(".//{%s}Assertion" % NS_A).getparent().getparent()) if False else None
evil = copy.deepcopy(ext[0]); 
for sig in evil.findall("{%s}Signature" % NS_DS): evil.remove(sig)
evil.set("ID", "_evil2"); evil.find("{%s}Subject/{%s}NameID" % (NS_A, NS_A)).text = "admin@example.com"
r.append(evil); write("xsw_signed_in_extensions", r)
# a signature on the Response that covers something else
r = reparse(response()); sign(assertion(r)); sig = r.find(".//{%s}Signature" % NS_DS)
ref = sig.find(".//{%s}Reference" % NS_DS); ref.set("URI", "#_nothing"); write("bad_reference", r)
# a doctype
data = open(os.path.join(HERE, "assertion_signed.xml"), "rb").read()
open(os.path.join(HERE, "doctype.xml"), "wb").write(b'<!DOCTYPE foo [<!ENTITY x "y">]>' + data)
# an encrypted assertion
r = reparse(response()); sign(r); r.append(etree.Element("{%s}EncryptedAssertion" % NS_A)); write("encrypted", r)

# a ds:Reference with an extra transform that is not allowed
r = reparse(response()); sign(assertion(r)); tr = r.find(".//{%s}Transforms" % NS_DS)
sub(tr, NS_DS, "Transform", Algorithm="http://www.w3.org/TR/1999/REC-xslt-19991116"); write("extra_transform", r)

# IdP metadata
md = ('<?xml version="1.0"?><md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="%s" entityID="%s"><md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">'
      '<md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor>'
      '<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST" Location="https://idp.example/sso-post"/>'
      '<md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example/sso"/>'
      '</md:IDPSSODescriptor></md:EntityDescriptor>') % (NS_DS, IDP, base64.b64encode(IDPCERT.public_bytes(serialization.Encoding.DER)).decode())
open(os.path.join(HERE, "idp_metadata.xml"), "w").write(md)
print("written")
