<?php
declare(strict_types=1);

namespace App\Form\Data;

use EWZ\Bundle\RecaptchaBundle\Validator\Constraints as ReCaptcha;
use Symfony\Component\Validator\Constraints as Assert;

class PasswordResetRequestData
{
    #[Assert\Email]
    #[Assert\NotBlank]
    public string $email;

    #[ReCaptcha\IsTrueV3]
    public string $recaptcha;
}
