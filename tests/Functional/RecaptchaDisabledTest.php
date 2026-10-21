<?php

declare(strict_types=1);

namespace App\Tests\Functional;

use Karser\Recaptcha3Bundle\Validator\Constraints\Recaptcha3Validator;
use Symfony\Bundle\FrameworkBundle\Test\KernelTestCase;

class RecaptchaDisabledTest extends KernelTestCase
{
    public function testRecaptchaValidatorConstructsWithoutRealKeys(): void
    {
        self::bootKernel();

        // The reCAPTCHA verifier is constructor-injected into the constraint
        // validator, so the validator service can only be built when the
        // configured secret is non-empty — even though captcha is disabled in
        // dev/test and .env ships empty keys by default.
        $validator = static::getContainer()->get('karser_recaptcha3.validator');

        self::assertInstanceOf(Recaptcha3Validator::class, $validator);
    }
}
